# ADR-0046 — The fleet's context budget: cap at the source, always with a way back

- **Status:** Accepted (amended 2026-08-20, issue #229 — see the
  amendment at the end)
- **Date:** 2026-08-13
- **Relates to:** [ADR-0039](0039-e2e-pod-is-the-worker-sandbox.md) (whose
  routing decision is the root cause), [ADR-0021](0021-continuous-streaming-session.md)
  / [ADR-0029](0029-session-is-the-durable-unit.md) (the unbounded session this
  operates on), [ADR-0009](0009-rtk-ponytail-baked-into-worker-image.md) (rtk,
  the fleet's only prior token measure), [ADR-0044](0044-e2e-pod-outlives-the-app.md)
  (static tool registration, left intact)

## Context

Worker sessions run out of context fast. The fleet had no measurement of this
and no mechanism against it beyond `rtk`, which shrinks a *command's output* at
the moment it is produced and does nothing about what has already accumulated.

Claude Code itself has five layers for this. They were read directly out of the
CLI the Agent SDK vendors at
`worker/node_modules/@anthropic-ai/claude-agent-sdk/cli.js` — the public
`anthropics/claude-code` repo is issues and plugins only, with no source.

| Layer | Mechanism | Fleet status before this ADR |
|---|---|---|
| 1 | Per-tool ingest caps: `Bash` 30000 chars, `Task` 30000, **MCP 25000 tokens**, `Read` 2000 lines, `WebFetch` 100000 chars | Defaults only — nothing tuned |
| 2 | **Microcompaction** — silently drops stale tool results mid-session; keeps last 3, targets ≤40000 tokens retained, fires only above the warning threshold and only if it saves ≥20000 | **Structurally inapplicable, see below** |
| 3 | Server-side context editing (`clear_tool_uses_20250919`, beta `context-management-2025-06-27`) | Env-gated, **off** |
| 4 | Auto-compact — summarize everything, emit `compact_boundary` with `pre_tokens` | On, unobserved |
| 5 | `Task` subagents (separate context window, final message only) | Allowed, never encouraged |

### The root cause

Microcompaction operates on a **hardcoded tool set**: `Read, Bash, Grep, Glob,
WebSearch, WebFetch, Edit, Write`. No MCP tool is in it. Neither is `Task`.

ADR-0039 routed every build, test, lint and install through `run_command` — an
MCP tool — for good reasons that still hold (the sandbox has no git and no
fleet credentials, which is what justifies it being un-prompted). The
unintended consequence: **the fleet's single largest context consumer is
precisely the one Claude Code's primary shedding mechanism cannot touch.** A
local Claude Code session drops a 200KB `bun install` log automatically once it
is three tool calls old. A worker carries it until auto-compact summarizes the
entire conversation away.

Measured secondary costs: ~8–9k tokens of fixed per-turn overhead before any
work (≈4k of Playwright tool schemas, mostly `inputSchema`; ~7k bytes of
first-party tool descriptions; 9.4KB `fleet-shared/CLAUDE.md`), and no token
metric anywhere — `usage` was written to the transcript at
`worker/src/session.ts` and read by nothing.

## Decision

**1. Cap every unbounded tool result at the source, and never cap without
returning a way to reach the rest.**

This is the load-bearing half. Claude Code's own MCP truncation notice reads
"If this MCP server provides pagination or filtering tools, use them to
retrieve specific portions of the data" — an instruction addressed to the
server author. A cap without a stated recovery path converts "context
explodes" into "agent reasons from partial build output and doesn't know it",
which is strictly worse: the first failure is loud, the second is silent.

- `run_command` (`e2e-runner/cmd/execmcp`) keeps the first 15000 bytes per
  stream and tees the **complete, interleaved** stdout+stderr to
  `/tmp/run-*.log` in the sandbox. The result carries `truncated`,
  `droppedBytes` and `fullOutputPath`; the agent recovers with an ordinary
  `tail`/`grep` through the same tool. `/tmp` persists across calls in that
  container — ADR-0044 already relies on this for `/tmp/e2e-app.log`. No new
  MCP tool, no new RPC, no cursor to maintain.
- `view_logs` caps at 15000 bytes. **No `offset` cursor is added**: `limit`,
  `level`, `duration` and `start_time`/`end_time` already *are* the filtering
  tools the notice should point at, and a cursor would mean a proto change plus
  a core change to buy nothing they don't already do.
- `wait_for_messages` caps per entry (2000 bytes), not per batch, so one pasted
  stack trace can't crowd out the short directives after it.

**2. Tune the SDK's own knobs via env, since none are `Options` fields.**
Set in `provisioner/internal/k8s/pod.go`:

| Var | Value | Why |
|---|---|---|
| `MAX_MCP_OUTPUT_TOKENS` | `10000` | CLI default 25000 is bounded but far too generous for a result that never leaves |
| `USE_API_CLEAR_TOOL_USES` | `1` | Server-side clearing |
| `API_MAX_INPUT_TOKENS` | `120000` | Trigger earlier than the 180000 default; fleet sessions are long-lived and resumable |
| `API_TARGET_INPUT_TOKENS` | `40000` | Clear back down to this |

`USE_API_CLEAR_TOOL_USES` **and not** `USE_API_CLEAR_TOOL_RESULTS`. The two
variants differ in exactly the way that matters here: the chosen one sends
`exclude_tools: [Edit, Write, NotebookEdit]`, so everything not on that list —
**including MCP tools** — becomes clearable. The other sends
`clear_tool_inputs: [Bash, Glob, Grep, Read, WebFetch, WebSearch]`, naming only
built-ins, and would do nothing for `run_command`, i.e. nothing for the actual
problem.

**3. Measure.** `worker/src/session.ts` logs `inputTokens`, `outputTokens` and
`cacheReadInputTokens` on every result, and raises `compact_boundary` to `warn`
with its `pre_tokens`. These were already in hand and discarded.

**4. Push exploration into `Task` subagents** via `fleet-shared/CLAUDE.md`
guidance. `Task` was already in `allowedTools` and nothing told the agent to
reach for it.

**5. Leave static tool registration alone.** ADR-0044's finding stands — a tool
the agent cannot see is a tool that does not exist. First-party descriptions
were tightened (~1100 bytes, every incident-derived warning kept verbatim); the
Playwright snapshot is untouched, since its weight is `inputSchema` (13.4KB),
not prose (1.6KB across 24 tools), and that is structural.

## Consequences

- Truncation is now a **contract**, not a loss: every capped result names its
  recovery path. Any future cap that doesn't is a bug, not an optimization.
- The fleet gets its first queryable context metric. `inputTokens` climbing
  monotonically toward the compact threshold is now visible in Loki.
- `USE_API_CLEAR_TOOL_USES` is **unverified under subscription OAuth** — the
  beta may be rejected for non-API-key auth. Failure is silent and harmless
  (the request proceeds unedited), which is why layers 1 and 3 above do not
  depend on it. Confirm by the sawtooth in `inputTokens`, not by assuming.
  If it is rejected, source-side capping is what carries this.
- Fixed per-turn overhead is essentially unchanged (~275 tokens saved). The
  plan expected more; the measurement says the first-party descriptions are
  mostly load-bearing and the Playwright weight is structural. Recorded so the
  next person doesn't re-attempt it expecting a win.
- Not addressed: resume still reloads the full prior transcript from the PVC
  JSONL. A separate decision.

## Amendment, 2026-08-20 (issue #229)

The "Not addressed" bullet above used to also say the rtk rewrite was skipped
in `bypassPermissions`/`acceptEdits` modes. Every clause of that has since
rotted — `bypassPermissions` was deleted by
[ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md), `run_command` by
[ADR-0048](0048-one-session-one-pod-one-shared-home.md) §6, and the
`acceptEdits` half was true but vacuous, since a file edit carries no
`command` for rtk to look at.

Naming the wrong modes hid the real gap, which is that **point 1 never reached
the fleet's largest output source.** `rtk` ran from `canUseTool`, and an allow
rule in `fleet-shared/settings.json` removes `canUseTool` from the path
entirely. Every command that fills a context window — `go test`, `go build`,
`cargo build`, `golangci-lint`, `make test`, `pytest` — is on that list, so rtk
never saw one. It fired only on short, human-gated commands whose output a
human was reading anyway.

The resolution was to move the cap to the agent layer, not to widen it in the
runtime. `rtkRewrite` is deleted; `fleet-shared/CLAUDE.md` asks the agent to
type the prefix by default. Two reasons for that direction rather than
extending the runtime's coverage:

1. It was a permission-model defect, not only a coverage one. `canUseTool`
   posted the `permission_request` carrying the command the agent wrote, then
   handed the SDK the rewritten one as `updatedInput`. The human approved one
   command and a different one executed.
2. Extending it needs a second gate. The SDK discards a `PreToolUse` hook's
   `updatedInput` unless the hook also returns `permissionDecision: "allow"` —
   which is exactly the rule-list-the-SDK-re-interprets that ADR-0053 deleted.

Point 1's principle is unchanged and still correct: cap at the source, and never
cap without returning a way to reach the rest. What this amendment records is
that "the source" for `Bash` is the agent typing the command, and the runtime
could not reach it from there without lying to the human about what it approved.

Consequence to accept: rtk coverage narrows in the short term, and prefixed
commands match no permission rule (the SDK's wrapper stripping knows `timeout`,
`nice`, `nohup`, `xargs` — not `rtk`), so in `default` mode each one costs a
prompt. No `Bash(rtk:*)` allow rule was added to fix that: it would let
`rtk sudo …` skip `canUseTool` and route around `FLEET_ASK_RULES`, trading a
token optimization for the `rm`/`sudo` gate.

`Bash(rtk proxy:*)` was on that allow list already and is removed here for the
same reason — `rtk proxy <cmd>` runs its argument verbatim, so the rule was
that bypass, live, since ADR-0009. An allow rule on any command that takes
another command as its argument is a hole in every ask rule at once.

Measure with `rtk gain --history` in a worker pod and `inputTokens` per point 3;
if the instruction does not take, the escalation is a skill, still in the agent
layer.
