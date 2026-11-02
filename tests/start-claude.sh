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
exec /bin/sleep 300
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
printf '%s\0' --append-system-prompt "$(cat "$HOME/.config/roles/coordinator.md")" \
  --continue "$special" '' $'line\nbreak' >"$tmp/expected"
cmp "$tmp/expected" "$HOME/args"
[[ $(cat "$HOME/cwd") == "$tmp/work with spaces" ]] || fail "wrong working directory"
[[ ! -e "$tmp/work with spaces/SHOULD_NOT_EXIST" ]] || fail "arguments were evaluated"
pane=$(tmux display-message -p -t '=claude' '#{pane_id}:#{pane_pid}')
"$launcher" 'ignored arguments' >"$tmp/output"
grep -q 'reusing existing session' "$tmp/output"
[[ $(tmux display-message -p -t '=claude' '#{pane_id}:#{pane_pid}') == "$pane" ]] || fail "session replaced"
cmp "$tmp/expected" "$HOME/args"
tmux kill-session -t '=claude'

# Force two launches past the initial has-session check before either creates it.
mkdir "$HOME/race"
for n in 1 2; do
  # The child Bash expands these positional arguments.
  # shellcheck disable=SC2016
  tmux new-session -d -s "race-$n" /bin/bash -c \
    '"$1" >"$2" 2>&1; printf "%s\n" "$?" >"$3"' race \
    "$launcher" "$tmp/race-$n-output" "$tmp/race-$n-status"
done
for ((i = 0; i < 100; i++)); do
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
rm -r "$HOME/race"
tmux has-session -t '=claude'
tmux kill-session -t '=claude'

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
printf '%s\0' --append-system-prompt "$(cat "$repo/modes/research.md")" >"$tmp/expected"
cmp "$tmp/expected" "$HOME/args"
echo "PASS: launcher arguments, prompt, cwd, reuse/concurrent reuse, errors and setup installation"
