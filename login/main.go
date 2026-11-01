// claude-login is a web page for logging this VM's claude in to claude.ai
// from a phone: it starts `claude auth login` in tmux, shows the sign-in link
// to copy, and types in the code you paste back. Once the login takes, it
// runs start-claude.
//
// It trusts the X-ExeDev-Email header set by exe.dev's HTTPS proxy, so serve
// it only through that proxy, with the VM's sharing kept private.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	addr    = flag.String("addr", ":8001", "address to listen on")
	allow   = flag.String("allow", "", "comma-separated emails allowed to use the page (default: anyone exe.dev's proxy lets through)")
	email   = flag.String("email", "", "claude.ai account email to pass to claude auth login")
	starter = flag.String("start", "start-claude", "command to run once logged in")
)

// The tmux session the login waits in.
const session = "claude-login"

// Matches the sign-in URL in claude auth login's output.
var (
	urlRE    = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize[^\s[:cntrl:]]+`)
	remoteRE = regexp.MustCompile(`https://claude\.ai/code/[A-Za-z0-9_-]+`)
	paneRE   = regexp.MustCompile(`^%[0-9]+$`)
)

type server struct {
	mu     sync.Mutex // serializes logins, captures and consent attempts
	emails map[string]bool
	trust  *trustOffer
	// Never retry an uncertain send, even if the same prompt remains visible.
	triedPane string
}

type trustOffer struct {
	token, pane, paneID, screen, home string
	yes                               bool
	expires                           time.Time
}

type claudeView struct {
	State     string `json:"state"`
	Screen    string `json:"screen,omitempty"`
	Error     string `json:"error,omitempty"`
	Directory string `json:"directory,omitempty"`
	Token     string `json:"token,omitempty"`
	RemoteURL string `json:"remoteURL,omitempty"`
}

func main() {
	flag.Parse()
	s := &server{emails: map[string]bool{}}
	for _, e := range strings.Split(*allow, ",") {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			s.emails[e] = true
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/start", s.start)
	mux.HandleFunc("POST /api/code", s.code)
	mux.HandleFunc("POST /api/claude", s.startClaude)
	mux.HandleFunc("POST /api/trust", s.trustClaude)
	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.auth(mux)))
}

// auth admits only users exe.dev's proxy has signed in (and on the allow list,
// if there is one), and refuses cross-site writes.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := strings.ToLower(r.Header.Get("X-ExeDev-Email"))
		if email == "" {
			http.Redirect(w, r, "/__exe.dev/login?redirect="+r.URL.Path, http.StatusFound)
			return
		}
		if len(s.emails) > 0 && !s.emails[email] {
			http.Error(w, email+" isn't allowed here", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth := loggedIn()
	s.trust = nil
	view := claudeView{State: "attention"}
	if auth {
		view = s.inspectClaude()
	}
	writeJSON(w, map[string]any{"loggedIn": auth, "claude": view})
}

// start begins a fresh login and returns its sign-in URL.
func (s *server) start(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trust = nil
	exec.Command("tmux", "kill-session", "-t", "="+session).Run()
	// A wide window, so the URL isn't wrapped.
	args := []string{"new-session", "-d", "-s", session, "-x", "2000", "-y", "50", "claude", "auth", "login", "--claudeai"}
	if *email != "" {
		args = append(args, "--email", *email)
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		httpError(w, "starting claude auth login: %v: %s", err, out)
		return
	}
	for range 30 {
		if url := urlRE.FindString(screen()); url != "" {
			writeJSON(w, map[string]any{"url": url})
			return
		}
		time.Sleep(time.Second)
	}
	httpError(w, "claude auth login never showed a sign-in URL. Its screen:\n%s", screen())
}

// code types the pasted code into the waiting login, waits for it to take,
// then starts claude.
func (s *server) code(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		httpError(w, "no code")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trust = nil
	if exec.Command("tmux", "has-session", "-t", "="+session).Run() != nil {
		httpError(w, "no login is waiting; get a new sign-in link")
		return
	}
	exec.Command("tmux", "send-keys", "-t", "="+session+":", "-l", strings.TrimSpace(req.Code)).Run()
	exec.Command("tmux", "send-keys", "-t", "="+session+":", "Enter").Run()
	for range 30 {
		if loggedIn() {
			exec.Command("tmux", "kill-session", "-t", "="+session).Run()
			s.launchClaude(w, r)
			return
		}
		time.Sleep(2 * time.Second)
	}
	httpError(w, "the login didn't take; the code may be wrong or expired. Its screen:\n%s", screen())
}

// startClaude retries launching for an already authenticated account, without
// another OAuth login. It shares the post-login path and its serialization.
func (s *server) startClaude(w http.ResponseWriter, r *http.Request) {
	if !s.mu.TryLock() {
		httpError(w, "another login/start is in progress; retry shortly")
		return
	}
	defer s.mu.Unlock()
	s.trust = nil
	if !loggedIn() {
		httpError(w, "Claude isn't logged in; get a sign-in link first")
		return
	}
	s.launchClaude(w, r)
}

// launchClaude is called with s.mu held. "started" means the launcher found
// the process running, not that Remote Control is connected or prompts passed.
func (s *server) launchClaude(w http.ResponseWriter, r *http.Request) {
	s.trust = nil
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, *starter)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	result := map[string]any{"loggedIn": true, "started": err == nil, "output": string(out)}
	if err != nil {
		detail := fmt.Sprintf("%s: %v", *starter, err)
		if ctx.Err() != nil {
			detail += ": " + ctx.Err().Error()
		}
		result["error"] = detail
		log.Printf("%s", detail)
		result["claude"] = claudeView{State: "attention", Error: detail}
	} else {
		result["claude"] = s.inspectClaude()
	}
	writeJSON(w, result)
}

// Only the isolated interactive session is eligible for consent. OAuth's
// screen() below is deliberately unrelated. Bound both time and output size.
type boundedOutput struct{ buf bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > 32768 {
		return 0, fmt.Errorf("tmux output exceeds 32 KiB")
	}
	return b.buf.Write(p)
}

func (b *boundedOutput) String() string { return b.buf.String() }

func claudeTmux(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", "roles-claude"}, args...)...)
	cmd.WaitDelay = time.Second
	var out boundedOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// A pane ID alone can be reused after a server restart. Linux process start
// times bind it to both the tmux server and the process running in the pane.
func processIdentity(pid string) (string, error) {
	if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
		return "", fmt.Errorf("invalid process ID")
	}
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("invalid process start time")
	}
	return pid + ":" + fields[19], nil
}

func claudePane() (key, id string, err error) {
	out, err := claudeTmux("list-panes", "-s", "-t", "=claude", "-F",
		"#{pid}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_current_command}\t#{pane_width}\t#{pane_height}")
	if err != nil {
		return "", "", err
	}
	f := strings.Split(out, "\t")
	if len(f) != 7 || strings.Contains(out, "\n") || f[3] != "0" || f[4] != "claude" ||
		!paneRE.MatchString(f[1]) {
		return "", "", fmt.Errorf("expected one live Claude pane")
	}
	width, ew := strconv.Atoi(f[5])
	height, eh := strconv.Atoi(f[6])
	if ew != nil || eh != nil || width < 40 || width > 400 || height < 20 || height > 80 {
		return "", "", fmt.Errorf("unsupported terminal size; inspect Claude in a terminal")
	}
	serverID, err := processIdentity(f[0])
	if err != nil {
		return "", "", err
	}
	paneProcess, err := processIdentity(f[2])
	if err != nil {
		return "", "", err
	}
	return serverID + "/" + f[1] + "/" + paneProcess, f[1], nil
}

func claudeSnapshot() (pane, id, screen string, err error) {
	pane, id, err = claudePane()
	if err != nil {
		return
	}
	screen, err = claudeTmux("capture-pane", "-p", "-J", "-t", id, "-S", "0", "-E", "79")
	if err != nil {
		return
	}
	again, _, checkErr := claudePane()
	if checkErr != nil || again != pane {
		err = fmt.Errorf("Claude pane changed during capture; check again")
		return
	}
	err = noClaudeClients()
	return
}

func noClaudeClients() error {
	clients, err := claudeTmux("list-clients", "-F", "#{client_pid}")
	if err != nil {
		return err
	}
	if clients != "" {
		return fmt.Errorf("detach all roles-claude terminal clients before using web consent")
	}
	return nil
}

// Deliberately recognize only this known prompt, including its explanation,
// option order and selected option. New versions/prompts fail closed.
func workspaceTrust(screen, home string) (yes, ok bool) {
	var lines []string
	for _, line := range strings.Split(screen, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 && strings.Trim(lines[0], "─") == "" {
		lines = lines[1:]
	}
	if home == "" || !strings.HasPrefix(home, "/") || len(lines) < 3 ||
		lines[0] != "Accessing workspace:" || lines[1] != home {
		return false, false
	}
	tail := strings.Join(strings.Fields(strings.Join(lines[2:], " ")), " ")
	const explanation = "Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first. Claude Code'll be able to read, edit, and execute files here. Security guide "
	const footer = " Enter to confirm · Esc to cancel"
	switch tail {
	case explanation + "❯ No, exit Yes, I trust this folder" + footer:
		return false, true
	case explanation + "No, exit ❯ Yes, I trust this folder" + footer:
		return true, true
	}
	return false, false
}

// Called with s.mu held; no input is ever sent by a status/start/check.
func (s *server) inspectClaude() claudeView {
	pane, id, screen, err := claudeSnapshot()
	view := claudeView{State: "attention", Screen: screen}
	if err != nil {
		s.trust = nil
		view.Error = err.Error()
		return view
	}
	home := os.Getenv("HOME")
	yes, ok := workspaceTrust(screen, home)
	if !ok {
		s.trust = nil
		view.Error = "Review the terminal below for prompts. Only the known HOME trust prompt can be accepted here."
		// Extract only from the fresh capture and known active banner, never
		// the launcher's output. It is a link, not a readiness assertion.
		if strings.Contains(screen, "/remote-control is active · Continue here, on your phone, or at") &&
			!strings.Contains(screen, "Accessing workspace:") {
			view.RemoteURL = remoteRE.FindString(screen)
		}
		return view
	}
	if s.triedPane == pane {
		s.trust = nil
		view.Error = "Trust was already attempted in this pane. Check the terminal; web consent will not retry."
		return view
	}
	if s.trust == nil || s.trust.pane != pane || s.trust.screen != screen ||
		s.trust.home != home || !time.Now().Before(s.trust.expires) {
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			s.trust = nil
			view.Error = "Couldn't create a consent token; check again."
			return view
		}
		s.trust = &trustOffer{token: hex.EncodeToString(token[:]), pane: pane,
			paneID: id, screen: screen, home: home, yes: yes, expires: time.Now().Add(time.Minute)}
	}
	view.State, view.Directory, view.Token = "trust", home, s.trust.token
	return view
}

// Explicit, authenticated, one-shot consent. Never a generic keys endpoint.
func (s *server) trustClaude(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token string }
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	offer := s.trust
	s.trust = nil
	fail := func(message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": message})
	}
	if decodeErr != nil || offer == nil || req.Token == "" || req.Token != offer.token ||
		!time.Now().Before(offer.expires) {
		fail("Consent expired or was already used. Start / check Claude again.")
		return
	}
	pane, id, screen, err := claudeSnapshot()
	yes, known := workspaceTrust(screen, os.Getenv("HOME"))
	if err != nil || !known || pane != offer.pane || id != offer.paneID ||
		screen != offer.screen || os.Getenv("HOME") != offer.home || yes != offer.yes {
		fail("Claude's prompt or terminal changed. Start / check Claude again; no keys sent.")
		return
	}
	if err := noClaudeClients(); err != nil {
		fail(err.Error())
		return
	}
	// Consume before attempting input. A timeout/error might still mean tmux
	// delivered it: never automatically retry, or issue another offer here.
	s.triedPane = pane
	keys := []string{"send-keys", "-t", id}
	if !yes {
		keys = append(keys, "Down")
	}
	keys = append(keys, "Enter")
	if _, err := claudeTmux(keys...); err != nil {
		fail("Trust input may or may not have arrived. Inspect the terminal; web consent will not retry: " + err.Error())
		return
	}
	time.Sleep(300 * time.Millisecond) // one bounded refresh, not ongoing polling
	writeJSON(w, map[string]any{"loggedIn": true, "started": true, "trustSent": true, "claude": s.inspectClaude()})
}

func loggedIn() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "auth", "status")
	cmd.WaitDelay = time.Second
	return cmd.Run() == nil
}

func screen() string {
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", "="+session+":").Output()
	return strings.TrimSpace(string(out))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Claude login</title>
<style>
  :root { --bg: #fafaf9; --fg: #1c1917; --muted: #78716c; --line: #d6d3d1; --accent: #c2410c; --ok: #15803d; --err: #b91c1c; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #1c1917; --fg: #f5f5f4; --muted: #a8a29e; --line: #44403c; --accent: #fb923c; --ok: #4ade80; --err: #f87171; }
  }
  * { box-sizing: border-box; }
  body { margin: 0; padding: 24px 16px; background: var(--bg); color: var(--fg);
         font: 16px/1.5 system-ui, sans-serif; max-width: 560px; margin-inline: auto; }
  h1 { font-size: 1.4rem; margin: 0 0 4px; }
  p { color: var(--muted); margin: 0 0 20px; }
  .step { border-top: 1px solid var(--line); padding: 16px 0; }
  button { font: inherit; padding: 10px 16px; border-radius: 8px; border: 1px solid var(--line);
           background: var(--accent); color: #fff; border-color: transparent; }
  button.secondary { background: transparent; color: var(--fg); border-color: var(--line); }
  button:disabled { opacity: .5; }
  .row { display: flex; gap: 8px; margin-top: 8px; }
  input { flex: 1; min-width: 0; font: inherit; padding: 10px; border-radius: 8px;
          border: 1px solid var(--line); background: transparent; color: var(--fg); }
  a.url { display: block; word-break: break-all; font-size: .85rem; color: var(--accent); margin-top: 8px; }
  pre { white-space: pre-wrap; word-break: break-word; font-size: .8rem; color: var(--muted); }
  .ok { color: var(--ok); } .err { color: var(--err); }
  [hidden] { display: none !important; }
</style>
</head>
<body>
<h1>Claude login</h1>
<p id="state">Checking…</p>

<div class="step">
  <button id="launch" hidden>Start / check Claude</button>
  <button id="start">Get sign-in link</button>
  <div id="link" hidden>
    <a class="url" id="url" target="_blank" rel="noopener"></a>
    <div class="row">
      <button class="secondary" id="copy">Copy link</button>
    </div>
  </div>
</div>

<div class="step" id="codestep" hidden>
  <label for="code">Paste the code the sign-in page shows</label>
  <div class="row">
    <input id="code" autocomplete="off" autocapitalize="off" spellcheck="false">
    <button id="submit">Log in</button>
  </div>
</div>

<div class="step" id="truststep" hidden>
  <strong>Trust this folder?</strong>
  <pre id="directory"></pre>
  <p>Claude Code will be able to read, edit, and execute files here. Only continue if you trust this folder.</p>
  <button id="trust">Trust this folder and continue</button>
</div>

<pre id="msg"></pre>
<div class="step" id="remote" hidden>
  <a class="url" id="remoteurl" target="_blank" rel="noopener">Open visible remote session</a>
  <p>This link is visible in the current terminal, not proof of readiness. Review any prompts above.</p>
</div>

<script>
const $ = id => document.getElementById(id);
const msg = (text, cls) => { $('msg').textContent = text || ''; $('msg').className = cls || ''; };
let revision = 0, trustToken = '';

function clearClaude() {
  trustToken = '';
  $('truststep').hidden = true;
  $('remote').hidden = true;
  $('remoteurl').removeAttribute('href');
}

function signedIn(loggedIn) {
  $('state').textContent = loggedIn ? 'claude is logged in on this VM.' : 'claude isn\'t logged in on this VM yet.';
  $('state').className = loggedIn ? 'ok' : '';
  $('start').textContent = loggedIn ? 'Log in again' : 'Get sign-in link';
  $('launch').hidden = !loggedIn;
}

async function call(path, body) {
  const r = await fetch(path, body === undefined ? {} : {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body) });
  const j = await r.json().catch(() => ({error: r.status + ' ' + r.statusText}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

async function refresh() {
  const version = ++revision;
  const s = await call('/api/status');
  if (version !== revision) return;
  signedIn(s.loggedIn);
  if (s.loggedIn) launched(s);
}

function launched(r) {
  clearClaude();
  const view = r.claude || {};
  if (r.error || r.started === false) {
    msg('Claude did not start:\n' + (r.error || '') + '\n' + (r.output || ''), 'err');
    return;
  }
  if (view.state === 'trust' && view.token && view.directory) {
    trustToken = view.token;
    $('directory').textContent = view.directory;
    $('truststep').hidden = false;
  }
  msg((r.trustSent ? 'Trust confirmation sent. ' : '') +
      'Remote Control readiness is not verified.\n' + (view.error || '') + '\n' + (view.screen || ''));
  if (view.state !== 'trust' && /^https:\/\/claude\.ai\/code\/[A-Za-z0-9_-]+$/.test(view.remoteURL || '')) {
    $('remoteurl').href = view.remoteURL;
    $('remote').hidden = false;
  }
}

async function action(label, path, body, done) {
  const version = ++revision;
  clearClaude();
  $('link').hidden = true;
  const buttons = ['launch', 'start', 'submit', 'trust'];
  buttons.forEach(id => $(id).disabled = true);
  msg(label);
  try {
    const r = await call(path, body);
    if (version === revision) done(r);
  } catch (e) {
    if (version === revision) { clearClaude(); msg(e.message, 'err'); }
  } finally {
    if (version === revision) buttons.forEach(id => $(id).disabled = false);
  }
}

$('launch').onclick = () => action('Starting / checking Claude…', '/api/claude', {}, launched);
$('trust').onclick = () => action('Confirming this folder once…', '/api/trust', {token: trustToken}, launched);
$('start').onclick = () => action('Starting claude auth login…', '/api/start', {}, ({url}) => {
  $('url').href = url; $('url').textContent = url;
  $('link').hidden = false; $('codestep').hidden = false; msg('');
});

$('copy').onclick = async () => {
  try { await navigator.clipboard.writeText($('url').href); $('copy').textContent = 'Copied'; }
  catch { $('copy').textContent = 'Copy failed: long-press the link'; }
  setTimeout(() => $('copy').textContent = 'Copy link', 2000);
};

$('submit').onclick = () => action('Logging in…', '/api/code', {code: $('code').value}, r => {
  launched(r);
  signedIn(r.loggedIn);
  $('codestep').hidden = true; $('code').value = '';
});

refresh().catch(e => { if (revision === 1) { clearClaude(); msg(e.message, 'err'); } });
</script>
</body>
</html>
`
