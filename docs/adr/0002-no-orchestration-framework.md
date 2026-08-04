# ADR-0002: No orchestration framework (Hermes/OpenClaw rejected)

**Status:** Superseded by [0017](0017-single-session-planning-pipeline.md) (the proposer+critic pair it describes is gone; the "no orchestration framework" conclusion still holds)
**Date:** 2026-07-20 (design-v0.md's open question) — resolved by 2026-07-30 implementation

## Context

`design-v0.md` §5 evaluated two first-party orchestration frameworks for
the coordination layer: **OpenClaw** (personal AI assistant / multi-channel
gateway, Docker/SSH sandbox backends, no official container image or k8s
support) and **Hermes Agent** (Nous Research's orchestrator-worker
framework, non-blocking delegation, no official image or native k8s
either). Both were candidates for "top-level coordinator" or "Discord edge
ingress" roles. Neither ships a container image or has native Kubernetes
support — either choice means hand-rolling a Dockerfile and expecting
daemon-vs-pod lifecycle work, on top of an already fast-moving,
second-party-heavy ecosystem where only first-party GitHub repos were
treated as authoritative.

The MVP's actual coordination need turned out to be narrow: a proposer and
a critic session need to exchange messages and see Mohammad's replies. That
doesn't require a coordinator process at all — it requires a shared
transport (ADR-0001) and the two `query()` calls know their own task ID.

## Decision

No orchestration framework. The Discord bot is a thin, custom ingress
(`bot/src/index.ts`) that writes directly to Postgres and Redis. The worker
runs proposer and critic as two independent Agent SDK `query()` sessions in
the same process (`Promise.allSettled` over two `for await` loops in
`worker/src/planning.ts`), coordinating only through the shared Redis
transcript. Claude Code's own Agent SDK primitives (session resume, the
`query()` loop itself) turned out sufficient without Hermes or OpenClaw.

## Consequences

- No daemon/systemd-service lifecycle to manage; the worker is just a
  Bun process in a Kubernetes pod.
- No community/second-party dependency risk from a fast-moving ecosystem.
- This decision was scoped to the current 2-worker, single-target-per-pod
  fleet. `design-v0.md` §11.1 flagged orchestration as worth revisiting
  "once N agents must run concurrently" — if the fleet ever needs to
  coordinate many simultaneous cross-repo agents rather than one
  proposer+critic pair per task, this ADR should be revisited, not
  silently overridden.
