# ADR-0006: Git commit identity derived live from the authenticated bot account

**Status:** Accepted
**Date:** 2026-07-31

## Context

A fresh worker container has no git identity configured — `git commit`
fails without `user.name`/`user.email`. The obvious fix is to hardcode
them (e.g. as env vars or literal strings in `git.ts`), matching whatever
GitHub bot account currently holds `GH_TOKEN`.

## Decision

`configureGitAuth()` (`worker/src/git.ts`) derives identity dynamically:
after `gh auth setup-git` wires `GH_TOKEN` into git's credential helper, it
runs `gh api user --jq .login` against that same token and sets
`git config --global user.name/user.email` from the result
(`<login>@users.noreply.github.com` — works regardless of whether the
account's real email is public).

## Consequences

- If the bot GitHub account is ever rotated (new PAT, different account),
  commit identity stays correct automatically — no code change needed.
- One extra `gh api` call per worker startup; negligible cost against a
  long-lived pod.
- Commit authorship in target-repo history will always show whichever
  account's token was live at commit time — expected and desired, not a
  bug, since that's the actual acting identity.
