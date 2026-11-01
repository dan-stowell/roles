package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"syscall"
	"testing"
	"time"
)

func commandStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubPATH(t *testing.T, auth bool, launch string) string {
	t.Helper()
	dir := t.TempDir()
	status := "exit 1"
	if auth {
		status = `test "$1 $2" = "auth status"`
	}
	commandStub(t, dir, "claude", status)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	old := *starter
	*starter = commandStub(t, dir, "starter", launch)
	t.Cleanup(func() { *starter = old })
	return dir
}

func request(t *testing.T, handler http.HandlerFunc, path, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("response %d: %s: %v", w.Code, w.Body, err)
	}
	return result
}

func TestStartClaude(t *testing.T) {
	for _, tt := range []struct {
		name, script, wantError, wantOutput string
		auth, started                       bool
	}{
		{"success", "echo process-running", "", "process-running", true, true},
		{"not logged in", "exit 99", "isn't logged in", "", false, false},
		{"empty error output", "exit 7", "exit status 7", "", true, false},
		{"error output", "echo startup-failed >&2; exit 3", "exit status 3", "startup-failed", true, false},
		{"missing executable", "", "no such file", "", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := stubPATH(t, tt.auth, tt.script)
			if tt.name == "missing executable" {
				*starter = filepath.Join(dir, "missing-starter")
			}
			s := &server{}
			got := request(t, s.startClaude, "/api/claude", "{}")
			if tt.auth && (got["loggedIn"] != true || got["started"] != tt.started) {
				t.Fatalf("incorrect auth/process state: %v", got)
			}
			if tt.wantError != "" && !strings.Contains(stringValue(got["error"]), tt.wantError) {
				t.Fatalf("missing error %q: %v", tt.wantError, got)
			}
			if !strings.Contains(stringValue(got["output"]), tt.wantOutput) {
				t.Fatalf("missing output %q: %v", tt.wantOutput, got)
			}
		})
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func TestPostLoginUsesSameLauncher(t *testing.T) {
	dir := stubPATH(t, true, "exit 9")
	commandStub(t, dir, "tmux", "exit 0")
	s := &server{}
	got := request(t, s.code, "/api/code", `{"code":"test-code"}`)
	if got["loggedIn"] != true || got["started"] != false || !strings.Contains(stringValue(got["error"]), "exit status 9") {
		t.Fatalf("post-login launch error lost: %v", got)
	}
}

func TestLaunchTimeout(t *testing.T) {
	stubPATH(t, true, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/api/claude", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s := &server{}
	start := time.Now()
	s.startClaude(w, r)
	if time.Since(start) > 2*time.Second {
		t.Fatal("launch didn't stop on request timeout")
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["loggedIn"] != true || got["started"] != false || !strings.Contains(stringValue(got["error"]), "deadline exceeded") {
		t.Fatalf("incorrect timeout result: %v", got)
	}
}

func TestLaunchSerialized(t *testing.T) {
	dir := stubPATH(t, true, `mkdir "$LAUNCH_LOCK" || exit 9
sleep .1
rmdir "$LAUNCH_LOCK"`)
	t.Setenv("LAUNCH_LOCK", filepath.Join(dir, "lock"))
	s := &server{}
	done := make(chan map[string]any, 1)
	go func() { done <- request(t, s.startClaude, "/api/claude", "{}") }()
	waiting := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(filepath.Join(dir, "lock")); err == nil {
			waiting = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		t.Fatal("first launch never started")
	}
	got := request(t, s.startClaude, "/api/claude", "{}")
	if !strings.Contains(stringValue(got["error"]), "in progress") {
		t.Fatalf("concurrent launch wasn't rejected: %v", got)
	}
	if first := <-done; first["started"] != true {
		t.Fatalf("first launch failed: %v", first)
	}
}

func TestAuthProtectsLaunch(t *testing.T) {
	for _, tt := range []struct {
		name, email, site, content string
		want                       int
	}{
		{"proxy login required", "", "", "application/json", http.StatusFound},
		{"allow list", "other@example.com", "", "application/json", http.StatusForbidden},
		{"cross site", "owner@example.com", "cross-site", "application/json", http.StatusBadRequest},
		{"content type", "owner@example.com", "", "text/plain", http.StatusBadRequest},
		{"authenticated", "owner@example.com", "same-origin", "application/json", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubPATH(t, true, "echo running")
			s := &server{emails: map[string]bool{"owner@example.com": true}}
			r := httptest.NewRequest("POST", "/api/claude", strings.NewReader("{}"))
			r.Header.Set("X-ExeDev-Email", tt.email)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			r.Header.Set("Content-Type", tt.content)
			w := httptest.NewRecorder()
			s.auth(http.HandlerFunc(s.startClaude)).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
}

const trustScreen = `
────────────────────────────────────────────────────────────────────────────────
 Accessing workspace:

 HOME

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source
 project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 Security guide

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel
`

func TestWorkspaceTrust(t *testing.T) {
	for _, tt := range []struct {
		name, screen string
		yes, ok      bool
	}{
		{"No selected", trustScreen, false, true},
		{"Yes selected", strings.Replace(strings.Replace(trustScreen, "❯ No,", "  No,", 1), "  Yes,", "❯ Yes,", 1), true, true},
		{"wrong directory", strings.Replace(trustScreen, "HOME", "/elsewhere", 1), false, false},
		{"directory only substring", strings.Replace(trustScreen, "HOME", "HOME/child", 1), false, false},
		{"no selection", strings.Replace(trustScreen, "❯", " ", 1), false, false},
		{"both selected", strings.Replace(trustScreen, "  Yes,", "❯ Yes,", 1), false, false},
		{"unknown explanation", strings.Replace(trustScreen, "read, edit, and execute", "read", 1), false, false},
		{"new option", strings.Replace(trustScreen, "No, exit", "No, continue", 1), false, false},
		{"additional prompt", trustScreen + "\nApprove another action?", false, false},
		{"stale remote link", trustScreen + "\nhttps://claude.ai/code/old", false, false},
		{"plain text", "Please trust HOME", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			screen := strings.ReplaceAll(tt.screen, "HOME", "/home/test")
			yes, ok := workspaceTrust(screen, "/home/test")
			if yes != tt.yes || ok != tt.ok {
				t.Fatalf("got yes=%v ok=%v; want %v %v", yes, ok, tt.yes, tt.ok)
			}
		})
	}
}

func trustStub(t *testing.T) string {
	t.Helper()
	dir := stubPATH(t, true, "echo process-running")
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("TRUST_TEST", dir)
	commandStub(t, dir, "tmux", `
test "$1 $2" = "-L roles-claude" || exit 90
shift 2
printf '%s\n' "$*" >> "$TRUST_TEST/calls"
case $1 in
  list-panes) cat "$TRUST_TEST/pane";;
  capture-pane)
    cat "$TRUST_TEST/screen"
    if [ -f "$TRUST_TEST/replace-pane" ]; then cp "$TRUST_TEST/replace-pane" "$TRUST_TEST/pane"; fi;;
  list-clients) cat "$TRUST_TEST/clients";;
  send-keys)
    printf '%s\n' "$*" >> "$TRUST_TEST/sends"
    if [ -f "$TRUST_TEST/send-error" ]; then echo uncertain >&2; exit 9; fi
    if [ -f "$TRUST_TEST/after" ]; then cp "$TRUST_TEST/after" "$TRUST_TEST/screen"; fi;;
  *) exit 91;;
esac`)
	put(t, dir, "pane", fmtPane(os.Getpid(), "%17", "claude"))
	put(t, dir, "screen", strings.ReplaceAll(trustScreen, "HOME", os.Getenv("HOME")))
	put(t, dir, "clients", "")
	return dir
}

func fmtPane(pid int, pane, command string) string {
	p := strconv.Itoa(pid)
	return p + "\t" + pane + "\t" + p + "\t0\t" + command + "\t120\t40\n"
}

func put(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func sends(t *testing.T, dir string) string {
	t.Helper()
	out, err := os.ReadFile(filepath.Join(dir, "sends"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(out)
}

func consent(t *testing.T, s *server) string {
	t.Helper()
	view := s.inspectClaude()
	if view.State != "trust" || view.Directory != os.Getenv("HOME") || len(view.Token) != 64 {
		t.Fatalf("no consent offer: %+v", view)
	}
	return `{"token":"` + view.Token + `"}`
}

func TestTrustOnce(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(strconv.FormatBool(yes), func(t *testing.T) {
			dir := trustStub(t)
			if yes {
				screen := strings.ReplaceAll(trustScreen, "HOME", os.Getenv("HOME"))
				screen = strings.Replace(strings.Replace(screen, "❯ No,", "  No,", 1), "  Yes,", "❯ Yes,", 1)
				put(t, dir, "screen", screen)
			}
			s := &server{}
			body := consent(t, s)
			got := request(t, s.trustClaude, "/api/trust", body)
			if got["trustSent"] != true {
				t.Fatalf("trust failed: %v", got)
			}
			// The fixture deliberately leaves the prompt unchanged after sending.
			for range 3 {
				request(t, s.trustClaude, "/api/trust", body)
				request(t, s.startClaude, "/api/claude", "{}")
				if view := s.inspectClaude(); view.Token != "" {
					t.Fatalf("offered to retry the same pane: %+v", view)
				}
			}
			want := "send-keys -t %17 Down Enter\n"
			if yes {
				want = "send-keys -t %17 Enter\n"
			}
			if got := sends(t, dir); got != want {
				t.Fatalf("got input %q, want exactly %q", got, want)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if !strings.Contains(string(calls), "capture-pane -p -J -t %17 -S 0 -E 79\n") {
				t.Fatalf("capture didn't pin the pane: %s", calls)
			}
		})
	}
}

func TestTrustConcurrent(t *testing.T) {
	dir := trustStub(t)
	s := &server{}
	body := consent(t, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request(t, s.trustClaude, "/api/trust", body)
		}()
	}
	wg.Wait()
	if got := sends(t, dir); got != "send-keys -t %17 Down Enter\n" {
		t.Fatalf("concurrent consent sent %q", got)
	}
}

func TestTrustRechecks(t *testing.T) {
	for _, name := range []string{
		"wrong token", "expired", "new server", "new pane", "new process", "dead process",
		"wrong directory", "changed selection", "changed explanation", "attached client",
		"replacement during capture", "capture failure", "oversized capture", "HOME changed",
		"other server instance", "no token", "bad JSON",
	} {
		t.Run(name, func(t *testing.T) {
			dir := trustStub(t)
			s := &server{}
			body := consent(t, s)
			screen := s.trust.screen
			switch name {
			case "wrong token":
				body = `{"token":"wrong"}`
			case "expired":
				s.trust.expires = time.Now().Add(-time.Second)
			case "new server":
				pane := strings.Replace(fmtPane(os.Getpid(), "%17", "claude"), strconv.Itoa(os.Getpid()), strconv.Itoa(os.Getppid()), 1)
				put(t, dir, "pane", pane)
			case "new pane":
				put(t, dir, "pane", fmtPane(os.Getpid(), "%18", "claude"))
			case "new process":
				put(t, dir, "pane", fmtPane(os.Getppid(), "%17", "claude"))
			case "dead process":
				put(t, dir, "pane", fmtPane(999999999, "%17", "claude"))
			case "wrong directory":
				put(t, dir, "screen", strings.ReplaceAll(screen, os.Getenv("HOME"), "/elsewhere"))
			case "changed selection":
				put(t, dir, "screen", strings.Replace(strings.Replace(screen, "❯ No,", "  No,", 1), "  Yes,", "❯ Yes,", 1))
			case "changed explanation":
				put(t, dir, "screen", strings.Replace(screen, "execute", "delete", 1))
			case "attached client":
				put(t, dir, "clients", "123\n")
			case "replacement during capture":
				put(t, dir, "replace-pane", fmtPane(os.Getpid(), "%18", "claude"))
			case "capture failure":
				commandStub(t, dir, "tmux", "exit 7")
			case "oversized capture":
				put(t, dir, "screen", strings.Repeat("x", 40000))
			case "HOME changed":
				t.Setenv("HOME", "/elsewhere")
			case "other server instance":
				s = &server{}
			case "no token":
				body = "{}"
			case "bad JSON":
				body = "not-json"
			}
			got := request(t, s.trustClaude, "/api/trust", body)
			if got["error"] == nil || sends(t, dir) != "" || s.trust != nil {
				t.Fatalf("unsafe consent: %v; sends %q; offer %+v", got, sends(t, dir), s.trust)
			}
			request(t, s.trustClaude, "/api/trust", body)
			if sends(t, dir) != "" {
				t.Fatal("retry sent input")
			}
		})
	}
}

func TestTrustSendErrorIsNotRetried(t *testing.T) {
	dir := trustStub(t)
	s := &server{}
	body := consent(t, s)
	put(t, dir, "send-error", "")
	got := request(t, s.trustClaude, "/api/trust", body)
	if !strings.Contains(stringValue(got["error"]), "may or may not") {
		t.Fatalf("ambiguous send not reported: %v", got)
	}
	request(t, s.trustClaude, "/api/trust", body)
	if view := s.inspectClaude(); view.Token != "" {
		t.Fatalf("ambiguous send was offered again: %+v", view)
	}
	if sends(t, dir) != "send-keys -t %17 Down Enter\n" {
		t.Fatal("uncertain send was retried")
	}
}

func TestTrustUnknownClearsOffer(t *testing.T) {
	dir := trustStub(t)
	s := &server{}
	body := consent(t, s)
	put(t, dir, "screen", "Unknown prompt\nhttps://claude.ai/code/stale\n")
	view := s.inspectClaude()
	if view.State != "attention" || view.Token != "" || s.trust != nil {
		t.Fatalf("unknown prompt retained consent: %+v", view)
	}
	request(t, s.trustClaude, "/api/trust", body)
	if sends(t, dir) != "" {
		t.Fatal("stale token sent keys")
	}
}

func TestFreshRemoteLinkDoesNotAssertReadiness(t *testing.T) {
	dir := trustStub(t)
	s := &server{}
	consent(t, s)
	put(t, dir, "screen", "/remote-control is active · Continue here, on your phone, or at\nhttps://claude.ai/code/session_test\nUnknown prompt")
	view := s.inspectClaude()
	if view.State != "attention" || view.Token != "" || view.RemoteURL != "https://claude.ai/code/session_test" || s.trust != nil {
		t.Fatalf("wrong live link state: %+v", view)
	}
	put(t, dir, "screen", "Unknown prompt\nhttps://claude.ai/code/stale")
	if view := s.inspectClaude(); view.RemoteURL != "" || view.Token != "" {
		t.Fatalf("stale link survived unknown screen: %+v", view)
	}
	commandStub(t, dir, "tmux", "exit 9")
	if view := s.inspectClaude(); view.RemoteURL != "" || view.Token != "" {
		t.Fatalf("stale controls survived capture failure: %+v", view)
	}
}

func TestTmuxCaptureBounded(t *testing.T) {
	dir := trustStub(t)
	put(t, dir, "screen", strings.Repeat("x", 40000))
	if _, err := claudeTmux("capture-pane"); err == nil {
		t.Fatal("oversized capture succeeded")
	}
}

func TestAuthProtectsTrust(t *testing.T) {
	for _, tt := range []struct {
		name, email, site, content string
		want                       int
	}{
		{"proxy login required", "", "", "application/json", http.StatusFound},
		{"allow list", "other@example.com", "", "application/json", http.StatusForbidden},
		{"cross site", "owner@example.com", "cross-site", "application/json", http.StatusBadRequest},
		{"content type", "owner@example.com", "", "text/plain", http.StatusBadRequest},
		{"authenticated", "owner@example.com", "same-origin", "application/json", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := trustStub(t)
			s := &server{emails: map[string]bool{"owner@example.com": true}}
			body := consent(t, s)
			r := httptest.NewRequest("POST", "/api/trust", strings.NewReader(body))
			r.Header.Set("X-ExeDev-Email", tt.email)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			r.Header.Set("Content-Type", tt.content)
			w := httptest.NewRecorder()
			s.auth(http.HandlerFunc(s.trustClaude)).ServeHTTP(w, r)
			if w.Code != tt.want || (sends(t, dir) != "") != (tt.want == http.StatusOK) {
				t.Fatalf("status %d, want %d; sends %q: %s", w.Code, tt.want, sends(t, dir), w.Body)
			}
		})
	}
}

func sessionHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{".config/roles", ".local/bin", ".cache/roles"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile("../start-claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local/bin/start-claude"), src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config/roles/coordinator.md"), []byte("line one\nline two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, ".local/bin")+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("TERM", "xterm-256color")
	return home
}

func TestSessionCommand(t *testing.T) {
	home := sessionHome(t)
	commandStub(t, filepath.Join(home, ".local/bin"), "claude", `pwd > "$HOME/cwd"
printf '%s\0' "$@" > "$HOME/args"`)
	cmd := exec.Command("../start-claude", "--session")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session: %v: %s", err, out)
	}
	args, err := os.ReadFile(filepath.Join(home, "args"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--remote-control\x00--append-system-prompt\x00line one\nline two\x00"
	if string(args) != want {
		t.Fatalf("args %q, want %q", args, want)
	}
	cwd, err := os.ReadFile(filepath.Join(home, "cwd"))
	if err != nil || strings.TrimSpace(string(cwd)) != home {
		t.Fatalf("cwd %q: %v", cwd, err)
	}
}

// Exercise the real foreground tmux configuration, on a private socket. A
// config-only -D server will hang forever unless a first client triggers it.
func TestTmuxExitsWhenClaudeDies(t *testing.T) {
	home := sessionHome(t)
	commandStub(t, filepath.Join(home, ".local/bin"), "claude", "exec sleep 30")
	socket := filepath.Join(home, "tmux.sock")
	cmd := exec.Command("tmux", "-D", "-S", socket, "-f", "../claude.tmux.conf")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ready := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if exec.Command("tmux", "-S", socket, "has-session", "-t", "=claude").Run() == nil {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("tmux session never appeared")
	}
	pidOut, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", "=claude:", "#{pane_pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tmux exit: %v: %s", err, &output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground tmux stayed alive after Claude died")
	}
}
