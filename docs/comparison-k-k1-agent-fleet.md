# Two agent-fleets, same problems

A side-by-side of this repo and [k-k1/agent-fleet](https://github.com/k-k1/agent-fleet),
written to start a conversation between the two projects, not to review either.
Both started from the same itch: an agent that dies when you close the laptop is not
infrastructure. They then answered it for very different people, and ran into many of
the same walls on the way.

As of 2026-10-03. Their side describes commit
[`2497b6c`](https://github.com/k-k1/agent-fleet/tree/2497b6c04b9818b1cfbf79f0e5caec7427b076a4)
and is taken from their own docs and code; ours describes the code at `d50dba8` (v4.14.5).
Corrections are welcome, especially on their side.

## Two goals

**k-k1/agent-fleet** is a self-hosted operations layer that a company runs for its
members: one deployment per company, sized for tens to about a hundred users
([k1:0001][k1-0001]). It supports nine agent kinds — eight agent CLIs run as they are
(Claude Code, Codex, Copilot, Cursor, OpenCode and others) plus its own llama.cpp
harness — each member in a persistent workspace: a Docker container, a bubblewrap
sandbox, or on AWS.

**This repo** is a personal fleet on a three-node homelab Kubernetes cluster, for one
operator. It drives one agent, Claude, through the Agent SDK. Each session is one piece
of work, and a pull request is the intended result. The story of how it got this shape,
including the first design that had to be deleted, is in
[Running a fleet of Claude agents on my own cluster](https://blog.bnei.dev/blog/running-a-fleet-of-claude-agents-on-my-cluster).

Most of the differences below follow from that one difference in who it is for.

## Side by side

Their decision records are linked as *k1:NNNN*, ours as *af:NNNN*.

| Problem | k-k1/agent-fleet | This repo |
|---|---|---|
| **Unit of isolation** | One long-lived workspace per member, many sessions inside it ([k1:0104][k1-0104]); a new git worktree per session by default ([build/01][k1-01]) | A single-shot Kubernetes Job each time a session is warmed; each session keeps its own node-local volume with a real clone and dependency caches ([af:0048][af-0048], [af:0054][af-0054]); at most 5 live pods |
| **When may the box stop** | By default a session stops after 1 h idle and a workspace after 2 h; tenants can change both ([main.go][k1-main]). Only a machine at work keeps a workspace up; waiting on a human never does ([k1:0055][k1-0055] D1) | A pod is torn down after 30 min idle; one that never speaks is reclaimed in 3 min ([af:0040][af-0040]). A pending permission keeps the pod; a pending question does not ([af:0058][af-0058]) |
| **Delivery after the box is gone** | Unanswered questions and plans are carried over before folding and delivered as a new prompt; a pending permission is carried as a fact only ([k1:0055][k1-0055]) | A question is durable; its answer reaches the next pod as a new turn ([af:0050][af-0050], [af:0058][af-0058]). No equivalent for plans; a permission keeps its pod instead |
| **Human ↔ agent** | Claude and Antigravity run in their terminal UI, read through hooks and answered with key sequences ([build/92][k1-92]); the other kinds run through structured drivers by default. Slack or Discord threads per session, with answer buttons ([k1:0020][k1-0020]) | The SDK's `canUseTool` holds a prompt until a human answers it in the dashboard ([af:0029][af-0029]); in `auto` mode only `rm`, `sudo` and plan approval still reach a human ([af:0053][af-0053]). Decisions are answerable from the session list ([af:0042][af-0042]); notifications are outbound only |
| **Agent ↔ agent** | `send_to_peer_session` between a member's sessions; a session may steer only the sessions it started ([k1:0041][k1-0041], [k1:0073][k1-0073]) | `prompt_agent` / `wait_for_agent` through our hub ([af:0041][af-0041]), the caller identified by its session's lease ([af:0057][af-0057]) |
| **Knowing when work is done** | A dispatch ledger and a reconciler: settled only after two ticks with idle evidence and no busy evidence; unknown stays unknown ([k1:0035][k1-0035]) | Liveness derived from the stored row on every read, including a `done` state ([af:0040][af-0040]), plus a 60 s reconcile of pod state against Kubernetes; no stored completion and no check that a PR was opened |
| **Unattended work** | A scheduler wakes a stopped workspace and starts the run ([scheduler_wake.go][k1-sched]); a turn cut short by a usage limit resumes at reset | Alerts and schedules only file a *proposal*, which has no path to a pod until a human opens it ([af:0048][af-0048]). We require a human here because the fleet can act on the cluster it runs on |
| **Usage** | One token ledger by feature, agent and model; calls that report no tokens are counted as unmeasured, never as zero ([k1:0029][k1-0029]) | Token counts as Loki log fields and on the transcript row; missing counts stay absent rather than zero. No ledger, no per-feature view ([af:0047][af-0047]) |
| **Upstream drift** | Every CLI pinned; a daily watcher dispatches contract tests that drive the real CLI; "seen" and "tested" recorded separately ([build/10][k1-10]) | The SDK pinned to an exact version, and the image build fails if its bundled CLI differs; our tests use a fake SDK |

## Three exchanges worth more than a row

**A late answer is a new message.** Both projects hit the same fact from different
directions. You measured that `claude --resume` drops an unanswered tool call from the
conversation, so the question cannot come back ([k1:0055][k1-0055] D4). We disabled the
SDK's built-in question tool and use our own: it holds the question for up to 45 seconds,
then returns `pending`, and its description tells the agent to end its turn. The answer
arrives later as `Answer to your earlier question (seq N): …`. Your carried prompt does
two things ours does not. It repeats the question text, because the question itself has
dropped out of the conversation. And it says, in fixed wording checked by a test, not to
ask again and to continue with this answer ([session_carried.go][k1-carried],
[test][k1-carried-test]). We took both: our delivered answer now repeats each question
with its answer and ends with a fixed "do not ask again" line, checked by a test
([af:0060][af-0060]).

**A peer message must never answer a human's question.** Both projects enforce this. On
your side, a prompt is refused while a question, plan or permission is pending
([session_io.go][k1-io]), and decisions only go through their own endpoints. On ours,
peer messages are stored as discussion entries, and only a message from a human can
resolve a permission, a guard we added after a peer did resolve one in production
([af:0041][af-0041]). Both projects also had the same gap: a queued peer message lost if
the process stops. Yours, [#1255](https://github.com/k-k1/agent-fleet/issues/1255), was
closed on 2026-10-03, after the commit this page describes; ours is still an accepted
gap. One more thing we learned from your 2026-08-31 addendum to [k1:0041][k1-0041]:
Claude Code 2.1.251 re-enabled its native cross-session messaging despite the environment
variables that had blocked it, and you now block it through launch settings, with a
test. When we checked, our workers already had both tools: the server-side flag was on
for our account. We went the other way from you and opened the channel on purpose: both
tools run without a prompt, and inbound messages are accepted ([af:0060][af-0060]). The
trade-off is real. Every worker shares one account, so the channel reaches all of its
sessions and bypasses our transcript and lease checks.

**Pin the upstream, and say whether you tested it.** Your release watcher keeps "we saw
a new version" separate from "it passed the contract". We pin, and our build refuses a
mismatched CLI, but nothing tells us whether a new SDK still behaves the way our
permission gate assumes. Our blog post describes one SDK upgrade that silently changed
the permission evaluator underneath us; a contract test might have caught it.

## Ideas that travel

What we are taking from you:

- The question text and a fixed "do not ask again" sentence in a delivered answer, both
  checked by a test.
- Contract tests against the real upstream, with "seen" kept apart from "tested".
- A ledger where unmeasured usage is visibly unmeasured, broken down by feature.
- Blocking the native cross-session channel before it appears, not after.

What might be useful to you, as questions:

- Both projects found that a permission's yes/no can only reach the process that asked.
  You let the process go and carry the fact; we keep the pod. Which costs less in
  practice for you?
- `wait_for_agent` checks, for up to 45 seconds, whether another session is idle or
  blocked on a human, computed from liveness rather than from an event
  ([af:0041][af-0041]). A reply itself arrives as a new message. Would something like it
  fit your fleet graph?

## What we got wrong

- **Cleanup that reported success.** Our first design's branch sweep ran, reported
  success, and never removed a single worktree. It ended with the whole worktree
  lifecycle being deleted ([af:0048][af-0048]).
- **Answers written but never delivered.** For a while, a human's answer was stored, the
  badge cleared and the dashboard looked right, but the next pod never received it. Four
  separate defects, none visible from outside ([af:0058][af-0058]).
- **A stale tool description, and two tools the same fix missed.** Our question tool
  told the agent it "blocks (up to timeoutMs)" for weeks after [af:0058][af-0058] removed
  that argument. Fixing it turned up two more blocking tools that still took one, one of
  them defaulting past the 60-second limit that 0058 was about ([af:0060][af-0060]).
- **No real end-to-end check of delivery.** Our tests replace the SDK with a fake, so the
  path that failed above is still only covered by running it for real. We keep a list of
  the checks that passed while the system was broken:
  [verification-traps.md](./verification-traps.md).

The lesson we share: an operation that reports success without proving its effect. Your
email named the same thing from your side, and [k1:0045][k1-0045] (decision 31) records it.

## Questions for you

1. Has the "do not ask again" wording needed changing as Claude Code versions moved?
2. With a session limited to steering the sessions it started, have you needed one
   session to reach a sibling, and how did you handle it?
3. Does the dispatch ledger ever settle "done" on work a human would call unfinished,
   and how do you notice when it does?

[k1-0001]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0001-self-host-vs-saas.md
[k1-0020]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0020-chat-bridge.md
[k1-0029]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0029-usage-accounting.md
[k1-0035]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0035-session-report-v2-ledger.md
[k1-0041]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0041-cross-session-messaging.md
[k1-0045]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0045-ec2-persistent-workspace.md
[k1-0055]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0055-idle-stop-and-carried-interactions.md
[k1-0073]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0073-session-spawned-sessions.md
[k1-0104]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/decisions/0104-long-lived-member-workspace.md
[k1-01]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/build/01-architecture.md
[k1-92]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/build/92-driving-a-tui.md
[k1-10]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/docs/build/10-development.md
[k1-main]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/control-plane/main.go
[k1-sched]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/control-plane/scheduler_wake.go
[k1-io]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/workspace/agent/internal/sessionx/session_io.go
[k1-carried]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/workspace/agent/internal/sessionx/session_carried.go
[k1-carried-test]: https://github.com/k-k1/agent-fleet/blob/2497b6c04b9818b1cfbf79f0e5caec7427b076a4/workspace/agent/internal/sessionx/session_carried_test.go
[af-0029]: ./adr/0029-sessions-not-tasks-permission-prompt-not-approval-gate.md
[af-0040]: ./adr/0040-session-liveness-and-stall-guard.md
[af-0041]: ./adr/0041-inter-agent-prompting.md
[af-0042]: ./adr/0042-console-rewrite.md
[af-0047]: ./adr/0047-metrics-scoped-to-the-hubs.md
[af-0048]: ./adr/0048-one-session-one-pod-one-shared-home.md
[af-0050]: ./adr/0050-a-question-outlives-its-pod.md
[af-0053]: ./adr/0053-the-gate-is-canusetool-not-a-rule-list.md
[af-0054]: ./adr/0054-the-toolchain-stays-in-the-pod.md
[af-0057]: ./adr/0057-coreservice-authenticates-with-the-session-lease.md
[af-0058]: ./adr/0058-an-answer-wakes-the-session.md
[af-0060]: ./adr/0060-claude-codes-own-cross-session-channel-is-open.md
