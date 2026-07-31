# ADR-0009: `rtk` + `ponytail` baked into the worker image

**Status:** Accepted
**Date:** 2026-07-31

## Context

Mohammad's own local Claude Code setup runs with `rtk` (Rust Token Killer,
a Bash-tool-output compression proxy wired via a `PreToolUse` hook) and the
`ponytail` plugin (a lean-code persona). The worker's own Agent SDK
sessions (proposer, critic, and the implementation-phase resumed session)
are themselves Claude Code sessions with real Bash tool access to the
target repo — the same token-cost and over-engineering pressures apply to
them as to any interactive session.

## Decision

`worker/Dockerfile` installs both: `rtk`'s static musl binary from GitHub
releases, and `ponytail` via `claude plugin marketplace add` +
`claude plugin install`. `worker/claude-settings.json` — copied to
`/root/.claude/settings.json` in the image — wires `rtk hook claude` as a
`PreToolUse` hook on `Bash` and enables the `ponytail` plugin, the same
settings shape as Mohammad's own `~/.claude/settings.json`.

## Consequences

- Worker sessions get the same token savings on Bash-heavy operations
  (repo exploration, test runs) as Mohammad's local sessions.
- Worker-authored code inherits `ponytail`'s bias toward minimal,
  YAGNI-driven implementations — consistent with what a human reviewer
  (Mohammad) already expects from local Claude Code output, reducing
  review friction on opened PRs.
- Both tools are pulled from external sources (`rtk-ai/rtk` GitHub
  releases, `DietrichGebert/ponytail` marketplace) at image build time —
  a build will fail if either is unreachable; no vendored fallback exists.
