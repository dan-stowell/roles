#!/bin/bash
# Real tmux, but a private socket/HOME and fake Claude: no user sessions touched.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
real_tmux=$(command -v tmux)
socket=roles-start-claude-test-$$
cleanup() {
  "$real_tmux" -L "$socket" kill-server 2>/dev/null || true
  rm -r -- "$tmp"
}
trap cleanup EXIT
export HOME="$tmp/home with spaces" PATH="$tmp/bin:$PATH"
mkdir -p "$HOME/.config/roles" "$tmp/bin" "$tmp/work with spaces" "$tmp/empty" "$tmp/no-claude" "$tmp/no-deps"
printf 'Coordinator prompt\nQuotes: " $ ; ` \\ and spaces\n' >"$HOME/.config/roles/coordinator.md"
cat >"$tmp/bin/tmux" <<EOF
#!/bin/bash
if [[ -f "\$HOME/tmux-fail" ]]; then
  echo "simulated tmux startup error" >&2
  exit 23
fi
# Hold each launcher's first check until both have observed no session.
if [[ \$1 == has-session && -d "\$HOME/race" && ! -f "\$HOME/race/\$PPID" ]]; then
  touch "\$HOME/race/\$PPID"
  for ((i = 0; i < 100; i++)); do
    ready=("\$HOME/race/"*)
    if ((\${#ready[@]} == 2)); then exit 1; fi
    sleep 0.02
  done
  echo "race barrier timed out" >&2
  exit 99
fi
exec "$real_tmux" -L "$socket" -f /dev/null "\$@"
EOF
cat >"$tmp/bin/claude" <<'EOF'
#!/bin/bash
printf '%s\0' "$@" >"$HOME/args"
pwd >"$HOME/cwd"
exec python3 "$HOME/ui.py"
EOF
cat >"$HOME/ui.py" <<'EOF'
import os
import pathlib
import select
import time
import tty

home = pathlib.Path.home()
mode = (home / "ui-mode").read_text().strip() if (home / "ui-mode").exists() else "ready"
url = "https://claude.ai/code/session_test123"
ready = f"/remote-control is active\n{url}\n/rc active | bypass permissions on"
prompts = [
    "Choose the text style that looks best with your terminal\n1. Dark mode\nTo change this later, run /theme",
    "Security notes:\nPress Enter to continue",
    "Accessing workspace:\nQuick safety check: Is this a project you created or one you trust?\n1. No, exit\n2. Yes, I trust this folder",
    "WARNING: Claude Code running in Bypass Permissions mode\n1. No, exit\n2. Yes, I accept",
]
screens = {
    "ready": ready,
    "unknown": f"Unknown confirmation: authorize a new plugin?\n{url}",
    "partial": f"{ready}\nSecurity notes:\nUnrecognized options; do NOT continue",
    "auth": "Log in to Claude\nOpen https://claude.ai/oauth/authorize to sign in",
    "pending": f"Remote Control is pending\n{url}",
    "incomplete": f"/remote-control is active\n{url}\nbypass permissions on",
    "stale": f"{ready}\nRemote Control is connecting",
    "failed": f"{ready}\nRemote Control connection failed: test failure",
    "consent": f"{ready}\n{prompts[3]}",
    "blank": "",
}
tty.setraw(0)
keys = open(home / "keys", "ab", buffering=0)
if mode == "exit":
    print("Claude startup exited", flush=True)
    raise SystemExit(7)
index = 0
received = b""
transition = None

def draw(text):
    print("\033[2J\033[H" + text.replace("\n", "\r\n"), end="", flush=True)

if mode == "sequence":
    # A blank initial render must not fail immediately or receive keys.
    draw("")
    time.sleep(0.25)
draw(prompts[0] if mode == "sequence" else screens[mode])
while True:
    if transition is not None and time.monotonic() >= transition:
        index += 1
        received = b""
        transition = None
        draw(prompts[index] if index < len(prompts) else ready)
    if select.select([0], [], [], 0.02)[0]:
        data = os.read(0, 1024)
        keys.write(data)
        received += data
        if mode == "sequence" and index < len(prompts) and transition is None:
            expected = b"\r" if index < 2 else b"\x1b[B\r"
            if received == expected:
                # Keep the answered screen visible through several polling ticks.
                transition = time.monotonic() + 0.35
EOF
chmod +x "$tmp/bin/"*
ln -s "$tmp/bin/tmux" "$tmp/no-claude/tmux"
for tool in dirname readlink; do
  ln -s "$(command -v "$tool")" "$tmp/no-deps/$tool"
  ln -s "$(command -v "$tool")" "$tmp/no-claude/$tool"
done
launcher=$repo/start-claude

fail() {
  echo "FAIL: $*" >&2
  exit 1
}
expect_failure() {
  local expected=$1 status=0
  shift
  "$@" >"$tmp/output" 2>&1 || status=$?
  [[ $status == "$expected" ]] || fail "expected exit $expected, got $status: $(cat "$tmp/output")"
}

# Dependencies and configuration fail before creating a session.
expect_failure 1 env PATH="$tmp/empty" "$launcher"
grep -q 'tmux is required' "$tmp/output"
expect_failure 1 env PATH="$tmp/no-claude" "$launcher"
grep -q 'claude is required' "$tmp/output"
mv "$HOME/.config/roles/coordinator.md" "$tmp/coordinator.md"
expect_failure 1 "$launcher"
grep -q 'rerun setup' "$tmp/output"
mv "$tmp/coordinator.md" "$HOME/.config/roles/coordinator.md"
touch "$HOME/tmux-fail"
expect_failure 23 "$launcher"
grep -q 'simulated tmux startup error' "$tmp/output"
rm "$HOME/tmux-fail"

# A prefix match must not hide the absence of the exact session.
tmux new-session -d -s claude-other /bin/sleep 300
# shellcheck disable=SC2016
special='spaces "quotes" $dollar; $(touch SHOULD_NOT_EXIST)'
(
  cd "$tmp/work with spaces"
  "$launcher" --continue "$special" '' $'line\nbreak'
) >"$tmp/output"
grep -q 'tmux attach -t claude' "$tmp/output"
for ((i = 0; i < 100; i++)); do
  [[ -f "$HOME/args" ]] && break
  sleep 0.02
done
[[ -f "$HOME/args" ]] || fail "Claude did not run"
printf '%s\0' --remote-control --dangerously-skip-permissions \
  --append-system-prompt "$(cat "$HOME/.config/roles/coordinator.md")" \
  --continue "$special" '' $'line\nbreak' >"$tmp/expected"
cmp "$tmp/expected" "$HOME/args"
[[ $(cat "$HOME/cwd") == "$tmp/work with spaces" ]] || fail "wrong working directory"
[[ ! -e "$tmp/work with spaces/SHOULD_NOT_EXIST" ]] || fail "arguments were evaluated"
grep -q 'Remote Control: https://claude.ai/code/session_test123' "$tmp/output"
pane=$(tmux display-message -p -t '=claude:' '#{pane_id}:#{pane_pid}')
[[ $pane =~ ^%[0-9]+:[0-9]+$ ]] || fail "did not capture a real pane and PID"
printf unknown >"$HOME/ui-mode"
"$launcher" 'ignored arguments' >"$tmp/output"
grep -q 'reusing existing session' "$tmp/output"
[[ $(tmux display-message -p -t '=claude:' '#{pane_id}:#{pane_pid}') == "$pane" ]] || fail "session replaced"
cmp "$tmp/expected" "$HOME/args"
[[ ! -s "$HOME/keys" ]] || fail "launcher typed into ready/reused pane"
tmux kill-session -t '=claude'
rm "$HOME/ui-mode"

# Force two launches past the initial has-session check before either creates it.
printf sequence >"$HOME/ui-mode"
rm "$HOME/keys"
mkdir "$HOME/race"
for n in 1 2; do
  # The child Bash expands these positional arguments.
  # shellcheck disable=SC2016
  tmux new-session -d -s "race-$n" /bin/bash -c \
    '"$1" >"$2" 2>&1; printf "%s\n" "$?" >"$3"' race \
    "$launcher" "$tmp/race-$n-output" "$tmp/race-$n-status"
done
for ((i = 0; i < 250; i++)); do
  [[ -f "$tmp/race-1-status" && -f "$tmp/race-2-status" ]] && break
  sleep 0.02
done
for n in 1 2; do
  [[ -f "$tmp/race-$n-status" ]] || fail "concurrent launch $n did not finish"
  [[ $(cat "$tmp/race-$n-status") == 0 ]] || fail "concurrent launch $n failed: $(cat "$tmp/race-$n-output")"
  grep -q 'tmux attach -t claude' "$tmp/race-$n-output"
done
[[ $(grep -l 'started session' "$tmp/race-"*-output | wc -l) == 1 ]] || fail "expected one new session"
[[ $(grep -l 'reusing existing session' "$tmp/race-"*-output | wc -l) == 1 ]] || fail "expected one concurrent reuse"
printf '\r\r\033[B\r\033[B\r' >"$tmp/expected-keys"
cmp "$tmp/expected-keys" "$HOME/keys"
rm -r "$HOME/race"
rm "$HOME/ui-mode"
tmux has-session -t '=claude'
tmux kill-session -t '=claude'

# Auto-onboarding sends only the exact confirmation keys, once per screen.
printf sequence >"$HOME/ui-mode"
rm "$HOME/keys"
ROLES_START_CLAUDE_TIMEOUT=5 "$launcher" >"$tmp/output"
grep -q 'Remote Control: https://claude.ai/code/session_test123' "$tmp/output"
printf '\r\r\033[B\r\033[B\r' >"$tmp/expected-keys"
cmp "$tmp/expected-keys" "$HOME/keys"
tmux kill-session -t '=claude'

# Unknown/partial screens, blank render, and URLs without readiness get no keys.
# Error text must defeat even stale positive markers; a pending consent must
# not be mistaken for readiness either, and may be answered only once.
for mode in unknown partial auth blank pending incomplete stale failed consent exit; do
  printf '%s' "$mode" >"$HOME/ui-mode"
  rm -f "$HOME/keys"
  expect_failure 1 env ROLES_START_CLAUDE_TIMEOUT=1 "$launcher"
  grep -q 'Last captured screen:' "$tmp/output"
  grep -q 'Attach with: tmux attach -t claude' "$tmp/output"
  if [[ $mode != exit ]]; then
    tmux has-session -t '=claude' || fail "$mode session was not preserved"
    if [[ $mode == consent ]]; then
      printf '\033[B\r' >"$tmp/expected-keys"
      cmp "$tmp/expected-keys" "$HOME/keys"
    else
      [[ ! -s "$HOME/keys" ]] || fail "keys sent to $mode screen"
    fi
    case $mode in
      unknown) grep -q 'Unknown confirmation' "$tmp/output" ;;
      failed) grep -q 'Remote Control reported a failure' "$tmp/output" ;;
      *) grep -q 'startup timed out' "$tmp/output" ;;
    esac
    # A failed startup is still a reused session: no driver or new validation.
    saved_pane=$(tmux display-message -p -t '=claude:' '#{pane_id}:#{pane_pid}')
    ROLES_START_CLAUDE_TIMEOUT=invalid "$launcher" >"$tmp/reused-output"
    grep -q 'reusing existing session' "$tmp/reused-output"
    [[ $(tmux display-message -p -t '=claude:' '#{pane_id}:#{pane_pid}') == "$saved_pane" ]] || fail "failed session replaced"
    tmux kill-session -t '=claude'
  else
    grep -q 'Claude exited before Remote Control was ready' "$tmp/output"
  fi
done
rm "$HOME/ui-mode"
expect_failure 1 env ROLES_START_CLAUDE_TIMEOUT=invalid "$launcher"
grep -q 'must be 1–60 seconds' "$tmp/output"

# Run setup without system changes or a real build/service/Claude invocation.
cat >"$tmp/bin/sudo" <<'EOF'
#!/bin/bash
if [[ $1 == tee ]]; then cat >/dev/null; fi
EOF
cat >"$tmp/bin/go" <<'EOF'
#!/bin/bash
[[ $1 == build && $2 == -o ]] || exit 1
printf '#!/bin/sh\nexit 0\n' >"$3"
chmod +x "$3"
EOF
chmod +x "$tmp/bin/sudo" "$tmp/bin/go"
expect_failure 1 env PATH="$tmp/no-deps" /bin/bash "$repo/setup"
grep -q 'tmux is required' "$tmp/output"
expect_failure 1 env PATH="$tmp/no-claude" /bin/bash "$repo/setup"
grep -q 'claude is required' "$tmp/output"
"$repo/setup" --mode research >"$tmp/setup-output"
cmp "$repo/start-claude" "$HOME/.local/bin/start-claude"
[[ -x "$HOME/.local/bin/start-claude" ]] || fail "installed launcher is not executable"
cmp "$repo/modes/research.md" "$HOME/.config/roles/coordinator.md"
rm "$HOME/args"
"$HOME/.local/bin/start-claude" >"$tmp/output"
for ((i = 0; i < 100; i++)); do
  [[ -f "$HOME/args" ]] && break
  sleep 0.02
done
printf '%s\0' --remote-control --dangerously-skip-permissions \
  --append-system-prompt "$(cat "$repo/modes/research.md")" >"$tmp/expected"
cmp "$tmp/expected" "$HOME/args"
grep -q 'Remote Control: https://claude.ai/code/session_test123' "$tmp/output"
echo "PASS: onboarding, readiness, bounded failures, arguments, cwd, reuse/race and installation"
