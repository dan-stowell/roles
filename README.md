# roles

Roles, coordinator modes and VM notes for agents on an exe.dev VM, set up by
pointing Shelley at this repo:

```
Set up github.com/dan-stowell/roles on this VM, following its README.
```

Add "in research mode" for the research coordinator.

## For Shelley: setting this up

1. Clone this repo to `~/roles` (or `git -C ~/roles pull` if it's there).
2. Run `~/roles/setup`, adding `--mode research` if you were asked for
   research mode. It's safe to rerun.
3. Tell the user, in a few lines:
   - the login page URL it prints, for logging claude in from a phone;
   - that Shelley's own new conversations now coordinate these roles too, and
     that a Shelley model change it reports takes effect once Shelley
     restarts (`sudo systemctl restart shelley`), which ends this
     conversation, so leave that to the user;
   - any warnings it printed, such as missing git identity.

Don't edit the installed copies in `~/.claude` or `~/.config`; change this
repo and rerun `setup`.

## What's here

- `guidance.md`: the VM notes every agent reads. Everything builds with tools
  on the VM, following each repo's own docs.
- `roles/`: helper roles, as claude subagents. `implementer`, `verifier`,
  `adversary` and `ideator`.
- `modes/`: what the main agent does with them. `build` plans, implements,
  verifies and opens draft PRs; `research` generates and probes research
  paths and writes them up. `shelley.md` is how Shelley maps the roles onto
  its subagents.
- `login/`: a page at `https://VMNAME.exe.xyz:8001/` that starts
  `claude auth login`, shows the sign-in link to copy, and takes the code
  back. Set `ROLES_LOGIN_ALLOW` to a comma-separated email list before
  running `setup` to restrict it further than exe.dev's proxy does.
- `start-claude`: installed in `~/.local/bin`; starts or checks the persistent
  interactive Claude session, with Remote Control and the installed
  `~/.config/roles/coordinator.md` appended to its normal system prompt.

## Starting Claude

Setup needs `claude` (with `--remote-control` support), `tmux`, `go`, and
passwordless `sudo` for systemd. It installs/enables `roles-claude.service`,
without interrupting an existing Claude session or restarting Shelley.
After signing in on the login page, Claude starts automatically. If already
signed in, use **Start / check Claude** there, or run `~/.local/bin/start-claude`.
Retries reuse a running session; they don't repeat OAuth or create duplicates.

Claude always starts in `$HOME`, not the caller's repository, with normal
permissions. The login page recognizes its workspace safety prompt only for
that exact directory. Review the displayed path and read/edit/execute warning,
then explicitly tap **Trust this folder and continue** to accept it. Setup
and **Start / check Claude** never accept prompts automatically.

Consent expires after one minute and is one-shot, tied to the live pane and
exact prompt/selection. Attached terminal clients must detach first. If the
prompt changes, check again; if sending fails or the prompt remains after an
attempt, inspect the terminal instead of retrying consent. Capture and input
are best-effort: another local process can still change the terminal between
the final check and input. No internal Claude trust settings are edited.

A running process or visible remote session link is **not** proof that Remote
Control is ready. The page shows a fresh terminal snapshot on load, after
consent, or with **Start / check Claude**; it does not continuously poll.
Unrecognized prompts require terminal review; no automatic keys are sent.
If a prompt needs attention, attach from a terminal:

```
tmux -L roles-claude attach -t =claude
```

Review prompts yourself; use `/remote-control` to check/connect Remote Control
if needed. Detach with **Ctrl-b, then d** to leave Claude running. This is an
interactive session, not the standalone `claude remote-control` server, so it
can carry the coordinator prompt.

Systemd owns an isolated foreground tmux server, independent of the login
page and your other tmux sessions. It starts on reboot, but does not
auto-restart after Claude exits/crashes; run the launcher to retry.
`sudo systemctl stop roles-claude` stops it.
After rerunning setup (including changing coordinator mode), an existing
session keeps its old prompt until you deliberately run
`sudo systemctl restart roles-claude`. Restarting starts a new conversation.
Startup failures include service/terminal diagnostics; inspect
`journalctl -u roles-claude` too. The private
`~/.cache/roles/claude.log` contains terminal output (possibly chat text or
session URLs), and is cleared at each service start.

Development checks:

```
bash -n setup start-claude
shellcheck setup start-claude
go -C login test -race ./...
go -C login build ./...
go -C login vet ./...
```
