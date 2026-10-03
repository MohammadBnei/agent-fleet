# ADR-0060 — Claude Code's own cross-session channel stays closed

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** [ADR-0041](0041-inter-agent-prompting.md) (the fleet's own
  inter-session channel, which this protects),
  [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md) (lease
  auth, which a native channel would bypass),
  [ADR-0058](0058-an-answer-wakes-the-session.md) (the answer-delivery path
  touched below), `docs/comparison-k-k1-agent-fleet.md` (where these gaps were
  found)

## Context

Claude Code ships its own agent-to-agent channel: a `ListAgents` tool, a
`SendMessage` tool, and an inbound side controlled by the `crossSessionInbound`
setting. All three are in the CLI the worker pins (2.1.237, SDK 0.3.237):
`Settings.crossSessionInbound?: 'accept' | 'hold' | 'refuse'` is in `sdk.d.ts`,
and both tool names are in the native binary.

**They were already live in our workers.** The binary gates them on
`CLAUDE_CODE_HARBOR_KITE` or a server-side flag (`tengu_harbor_kite`). We
assumed that flag was off. A worker started on 2026-10-03 from `main`, with the
fleet's real token and **no** env flag, listed both `SendMessage` and
`ListAgents` in its `system/init` tool list. So the server-side flag is on for
this account, and every fleet session could already see them. k-k1 hit the
same thing: their ADR 0041 addendum (2026-08-31) records an env-var block that
held until CLI 2.1.251, which then delivered a message anyway.

For this fleet it matters more than for most. Every worker pod runs on the
same `CLAUDE_CODE_OAUTH_TOKEN`, so to the CLI every other fleet session is
"your other session". With the inbound setting unset, the CLI auto-delivers a
peer message whenever both sessions are in the same permission-mode class. A
message on that path would reach a session around everything ADR-0041 built:
not written to the `transcript`, not authorized by lease (ADR-0057), not fenced
as peer input, and not subject to the "a peer can never resolve a human's
decision" guard.

`canUseTool` does not cover it. The fleet's own tools and everything in `auto`
mode are allowed without a human (ADR-0053), so a gate on the tool call alone
would not stop it.

## Decision

1. `SendMessage` and `ListAgents` are in the worker's `disallowedTools`, beside
   the built-in `AskUserQuestion`. That closes the outbound side.
2. The per-session SDK `settings` carry `crossSessionInbound: "refuse"` and
   `remoteControlAtStartup: false`. An explicit value always wins over the
   CLI's mode-parity default, so inbound delivery is closed whatever the flag
   does.
3. Inter-session communication stays on `prompt_agent` (ADR-0041), which core
   records, authorizes and fences.

A test in `worker/src/session.test.ts` pins all four values.

## Consequences

- **Cost: an agent cannot message or continue its own `Task` subagents.** The
  binary's `SendMessage` also covers "agent teammates" (its own search hint),
  and `ListAgents` lists in-process subagents as well as other sessions.
  Subagents still run and return their result; only follow-up messaging is
  lost. Accepted: nothing in the fleet uses it, and a channel around core is a
  worse problem than a missing convenience.
- **Checked live, not only in a unit test.** A harness ran the real worker,
  sidecar and CLI 2.1.237 against a local core. In the `system/init` tool list:
  `main` without the env flag had both tools (47 tools); `main` with
  `CLAUDE_CODE_HARBOR_KITE=1` had both; this change with the flag had neither
  (45 tools). The inbound `refuse` was not exercised live: that would mean
  sending a real cross-session message. It is pinned by the config test.
- **Re-check on every CLI bump.** New releases can rename the tools or add a
  setting (0.3.288 adds `isolatePeerMachines`). The SDK bump that follows this
  ADR re-runs the same check.

## Also in this change (found in the same comparison)

- **A delivered answer now carries its question and a fixed "do not re-ask"
  line** (`formatAnswerTurn` in `worker/src/session.ts`). After a resume the
  question may no longer be in the conversation, and k-k1 measured that without
  fixed wording Claude asks again (their ADR 0055 D4/D5). No lookup is needed:
  the dashboard already writes an answer as
  `{"answers": {"<question text>": "<label>"}}`. The line is worded to be
  harmless on the known double delivery (a live poll and the warm path can both
  deliver one answer).
- **No blocking sidecar tool takes a timeout any more.** ADR-0058 removed
  `timeoutMs` from `AskUserQuestion` but left it on `wait_for_messages` and
  `wait_for_agent`. `wait_for_agent` defaulted to 120000, so a call with no
  arguments crossed the 60s ceiling: the 0058 incident again. All three now use
  one fixed 45s wait (`blockingWaitMs`). The descriptions build the number from
  that constant, so they cannot go stale the way "Blocks (up to timeoutMs)" did.
  ADR-0041 is amended to match.
- **The sidecar's per-call ceiling is now set by the worker**
  (`SIDECAR_MCP_TIMEOUT_MS`, 90s, in the MCP server config) rather than left
  to the CLI default, so a CLI upgrade cannot move it silently.
