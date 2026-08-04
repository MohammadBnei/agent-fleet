# agent-fleet-planning

Local Claude Code plugin, loaded by the worker's planning-phase `query()`
call via `plugins: [{ type: "local", path: "..." }]` (see
`worker/src/planning.ts`, `docs/adr/0017`).

`skills/doubt-driven-development/SKILL.md` and
`skills/architecture-interview/SKILL.md` are **manual, point-in-time
copies** of `~/.claude/skills/<name>/SKILL.md`. No automated sync exists —
these are static instruction files, and a stale copy fails safe (worse
guidance, not broken behavior). Re-copy by hand if the upstream skill
changes meaningfully. Same tradeoff this repo already made for `ponytail`
in `worker/Dockerfile` (see `docs/adr/0009`).
