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
