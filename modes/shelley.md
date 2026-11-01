## Helpers in Shelley

Your helpers are subagents you start with your subagent tool. A subagent sees
neither this conversation nor the VM notes, so begin every subagent's first
message by telling it to read `~/.config/shelley/AGENTS.md` (the VM notes) and
its role file, `~/.claude/agents/ROLE.md` (`implementer`, `verifier`,
`adversary` or `ideator`), and to follow both. Ignore the front matter in the
role files. It's for claude, but an adversary or ideator must still not edit
files. Then give it the task and all the context it needs, and ask for the
role file's report when it's done. Use only GPT models: leave the subagent's
model unset so it runs on yours, or pick a GPT model no weaker than yours, and
never give it lower reasoning than your own. Use a new subagent for each fresh
look rather than reusing one that has already seen the work.
