# VM notes

This is a throwaway exe.dev dev VM. Work in the repos cloned into $HOME, or
clone what you're pointed at there.

- Give each separate piece of work its own short, descriptive kebab-case
  branch (`fix-cache-eviction-race`, no prefix), cut from the default branch,
  rather than piling it onto one branch.
- Build, test and lint with the tools on this VM, the way the repo's own docs
  (README, CONTRIBUTING, Makefile, CI config) say to. Everything runs here:
  don't reach for remote build services such as `bb remote` unless asked. If a
  tool the repo needs is missing, install it rather than skipping the step.
  Never skip building or running tests, and don't tell helpers to just read
  code instead.
- Follow the repo's layout and conventions. Add tests to the existing test
  file that covers the code, and new code to the file it belongs in, rather
  than starting new files or packages for them.
- Commit and push often.
- When a branch is worth looking at, open a draft PR with
  `gh pr create --draft` and keep it updated by pushing. Only ever draft:
  never run `gh pr ready` or `gh pr merge`, never request reviewers, and never
  mark a PR ready some other way. Dan does all of that. Keep the body short:
  a one-line headline plus at most 2-3 one-sentence bullets. Don't add labels
  to PRs.
- Keep issues short: a clear, concrete title and at most 2-3 one-line bullets
  in the body. Label every issue you open `agent-written` if the repo has that
  label, plus any others from `gh label list` that fit.
- Never reply to issue or PR comments written by humans; Dan answers those.
- If a push or `gh` fails for lack of access, say so rather than working
  around it.
