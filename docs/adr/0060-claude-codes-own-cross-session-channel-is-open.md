# ADR-0060 — Claude Code's own cross-session channel is open

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** [ADR-0041](0041-inter-agent-prompting.md) (the fleet's own
  inter-session channel, which stays),
  [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md) (lease
  auth, which this channel does not pass through),
  [ADR-0058](0058-an-answer-wakes-the-session.md) (the answer-delivery path
  touched below), `docs/comparison-k-k1-agent-fleet.md` (where these gaps were
  found)

## Context

Claude Code ships its own agent-to-agent channel: a `ListAgents` tool, a
`SendMessage` tool, and an inbound side controlled by the `crossSessionInbound`
setting (`'accept' | 'hold' | 'refuse'`). All three are in the CLI the worker
pins (2.1.237, SDK 0.3.237).

**They were already live in our workers.** The binary gates the tools on
`CLAUDE_CODE_HARBOR_KITE` or a server-side flag (`tengu_harbor_kite`). A worker
started on 2026-10-03 from `main`, with the fleet's real token and **no** env
flag, listed both tools in its `system/init` tool list, so the server-side flag
is on for this account. Nothing in the fleet had decided that. The tools also
prompted a human on every call, since neither was in `allowedTools`, and the
inbound side ran on the CLI's unset default.

k-k1/agent-fleet chose the opposite of this ADR: they block the channel in
launch settings (their ADR 0041 addendum, 2026-08-31). This was considered here
first. A first draft of this change disallowed both tools and refused inbound
delivery. Mohammad overruled it: fleet sessions should be able to use the
channel themselves.

## Decision

1. `SendMessage` and `ListAgents` are in the worker's `allowedTools`, so an
   agent uses them without a human prompt.
2. The per-session SDK `settings` carry `crossSessionInbound: "accept"`. An
   explicit value, because the unset default **holds** a message from a session
   in a different permission mode, and nobody can approve a held message in a
   headless pod.
3. `remoteControlAtStartup: false` stays. That is phone/web control of the pod,
   a different channel.
4. `prompt_agent` ([0041](0041-inter-agent-prompting.md)) stays. It is still the
   only inter-session path that core records, authorizes and shows in the
   dashboard.

A test in `worker/src/session.test.ts` pins these values.

## Consequences

- **The peer set is the whole account, not the fleet.** Every worker uses the
  same `CLAUDE_CODE_OAUTH_TOKEN`, so `ListAgents` and `SendMessage` reach every
  session on it: other fleet pods and Mohammad's own laptop sessions, in both
  directions. Any of them can steer a fleet agent, and a fleet agent can steer
  them. Accepted deliberately.
- **This channel goes around the fleet.** A native message is not written to
  the `transcript`, not checked against a lease (0057), not fenced as peer input
  the way 0041's amendment fences `prompt_agent`, and not visible in the
  dashboard or Discord. Whether the worker relays an inbound native message into
  the transcript at all was not checked.
- **No loop cap.** `prompt_agent` refuses a blocked target and caps relay depth
  "so chains cannot loop" (`sidecar/internal/mcpserver/interagent.go`). The
  native channel has neither, and with both ends unprompted nothing stops it.
  Two pods can message each other indefinitely: each message starts a new turn,
  so neither goes idle, the idle teardown never fires, and both hold slots of
  the five-pod cap while spending the shared subscription. None of it reaches
  the transcript, so the dashboard and Discord cannot show it. Accepted
  deliberately (2026-10-03). If it bites, the cheapest fixes are a prompt on
  `SendMessage` (drop it from `allowedTools`) or a per-turn count of inbound
  native messages in the worker.
- **It cannot answer a human's decision.** A pending permission resolves only on
  a structured `permission_response` read from the transcript, and a native
  message never reaches the transcript. A peer can still talk the agent into
  doing something else, which `prompt_agent` could also do.
- **Re-check on every CLI bump.** New releases can rename the tools or change
  what the inbound values mean (0.3.288 adds `isolatePeerMachines`, which asks
  before sending to a peer on another machine).

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
