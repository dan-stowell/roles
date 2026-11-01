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
