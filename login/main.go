// claude-login is a web page for logging this VM's claude in to claude.ai
// from a phone: it starts `claude auth login` in tmux, shows the sign-in link
// to copy, and types in the code you paste back. Once the login takes, it
// runs start-claude.
//
// It trusts the X-ExeDev-Email header set by exe.dev's HTTPS proxy, so serve
// it only through that proxy, with the VM's sharing kept private.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
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
var urlRE = regexp.MustCompile(`https://claude\.com/cai/oauth/authorize[^\s[:cntrl:]]+`)

type server struct {
	mu     sync.Mutex // serializes logins
	emails map[string]bool
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
	writeJSON(w, map[string]any{"loggedIn": loggedIn()})
}

// start begins a fresh login and returns its sign-in URL.
func (s *server) start(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if !loggedIn() {
		httpError(w, "Claude isn't logged in; get a sign-in link first")
		return
	}
	s.launchClaude(w, r)
}

// launchClaude is called with s.mu held. "started" means the launcher found
// the process running, not that Remote Control is connected or prompts passed.
func (s *server) launchClaude(w http.ResponseWriter, r *http.Request) {
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
	}
	writeJSON(w, result)
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

<pre id="msg"></pre>

<script>
const $ = id => document.getElementById(id);
const msg = (text, cls) => { $('msg').textContent = text || ''; $('msg').className = cls || ''; };

async function call(path, body) {
  const r = await fetch(path, body === undefined ? {} : {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body) });
  const j = await r.json().catch(() => ({error: r.status + ' ' + r.statusText}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

async function refresh() {
  const s = await call('/api/status');
  $('state').textContent = s.loggedIn ? 'claude is logged in on this VM.' : 'claude isn\'t logged in on this VM yet.';
  $('state').className = s.loggedIn ? 'ok' : '';
  $('start').textContent = s.loggedIn ? 'Log in again' : 'Get sign-in link';
  $('launch').hidden = !s.loggedIn;
}

function launched(r) {
  msg((r.started ? 'Claude process is running (Remote Control may need attention).\n'
                 : 'Logged in, but Claude did not start:\n' + (r.error || '') + '\n')
      + (r.output || ''), r.started ? '' : 'err');
}

$('launch').onclick = async () => {
  $('launch').disabled = true; msg('Starting / checking Claude…');
  try { launched(await call('/api/claude', {})); }
  catch (e) { msg(e.message, 'err'); }
  $('launch').disabled = false;
};

$('start').onclick = async () => {
  $('start').disabled = true; msg('Starting claude auth login…');
  try {
    const {url} = await call('/api/start', {});
    $('url').href = url; $('url').textContent = url;
    $('link').hidden = false; $('codestep').hidden = false; msg('');
  } catch (e) { msg(e.message, 'err'); }
  $('start').disabled = false;
};

$('copy').onclick = async () => {
  try { await navigator.clipboard.writeText($('url').href); $('copy').textContent = 'Copied'; }
  catch { $('copy').textContent = 'Copy failed: long-press the link'; }
  setTimeout(() => $('copy').textContent = 'Copy link', 2000);
};

$('submit').onclick = async () => {
  $('submit').disabled = true; msg('Logging in…');
  try {
    const r = await call('/api/code', {code: $('code').value});
    launched(r);
    $('link').hidden = true; $('codestep').hidden = true; $('code').value = '';
    refresh();
  } catch (e) { msg(e.message, 'err'); }
  $('submit').disabled = false;
};

refresh().catch(e => msg(e.message, 'err'));
</script>
</body>
</html>
`
