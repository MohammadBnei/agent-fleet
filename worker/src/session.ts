import { query, type SDKUserMessage, type PermissionResult, type PermissionMode } from "@anthropic-ai/claude-agent-sdk";
import * as sidecar from "./sidecarClient.js";
import { log } from "./log.js";
import { rtkRewrite } from "./rtkHook.js";

// TransientError used to be thrown when an SDK result looked like a
// transient hiccup (0 turns, $0 cost), so the caller could leave the task
// for core's reclaim instead of failing it. Deleted in docs/adr/0048 with
// the reclaim itself: nothing re-dispatches a session, so there is no
// second outcome for a "transient" failure to have. It exits non-zero, the
// Job goes Failed, and a human sees CRASHED with the error attached.

const SESSION_ID = process.env.SESSION_ID ?? "";
const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const MAX_TURNS = process.env.MAX_TURNS ? Number(process.env.MAX_TURNS) : undefined;

const SIDECAR_MCP_ADDR = process.env.SIDECAR_MCP_ADDR ?? "localhost:9090";
const WORKTREE_PATH = process.env.WORKTREE_PATH ?? "/workspace";
// Set by the provisioner (task.SessionID, via CreateWorkerPodRequest) when
// this pod is warming an existing session rather than starting a fresh
// one — empty, not unset, for a brand-new task. Requires CLAUDE_CONFIG_DIR
// to already point at the shared PVC (provisioner/internal/k8s/pod.go) or
// there's nothing on this fresh container filesystem to resume from.
const RESUME_SESSION_ID = process.env.RESUME_SESSION_ID || undefined;
// The transcript seq to start streamHumanMessages from (provisioner/
// internal/k8s/pod.go's RESUME_FROM_SEQ) — one past whatever already
// existed for this task at dispatch time. 0 for a brand-new task.
// Without this, a fresh pod's cursor defaults to 0 and replays every
// pre-existing human directive, most critically a stale Stop's "abort"
// entry — self-aborting a resumed session within seconds of it starting
// (a real regression, caught live against a kind cluster).
const RESUME_FROM_SEQ = process.env.RESUME_FROM_SEQ ? Number(process.env.RESUME_FROM_SEQ) : 0;

function sidecarMcpServer() {
  return { type: "http" as const, url: `http://${SIDECAR_MCP_ADDR}/mcp` };
}

// Playwright, run by the SDK itself as a local stdio server in this pod
// (docs/adr/0048 §6).
//
// It used to be proxied: agent → sidecar → core → provisioner → e2e pod, with
// the tool list served from a snapshot committed into the sidecar because the
// real list could not be fetched reliably. docs/adr/0044 records what that
// cost — browser automation was dead for the fleet's entire history behind
// three stacked failures, each of which the layer above it happily passed.
//
// Declaring it here removes all three at once: the SDK owns the process, so
// the tool list is whatever the installed version actually exposes, there is
// no registration racing a pod that is still starting, and a failure surfaces
// as a real error instead of an empty result from a swallowed hop.
function playwrightMcpServer() {
  return {
    type: "stdio" as const,
    command: "playwright-mcp",
    // --browser chromium is NOT a preference, it is the fix from
    // docs/adr/0044: @playwright/mcp bundles its own playwright-core, and
    // without this it looks for BRANDED Chrome at /opt/google/chrome/chrome
    // and fails to start. The image installs the matching chrome-for-testing
    // build alongside it (see worker/Dockerfile) — the two halves have to
    // stay together.
    args: ["--headless", "--isolated", "--browser", "chromium"],
  };
}

// Feeds query()'s streaming-input mode — push() enqueues a message, the
// async generator yields it whenever the SDK asks for the next input.
// Verified in Phase 0's spike: the same Query object accepts further
// streamInput() calls after interrupt(), so this queue's generator just
// keeps running for the task's whole lifetime, never recreated.
class InputQueue {
  private queue: SDKUserMessage[] = [];
  private waiters: ((msg: SDKUserMessage) => void)[] = [];
  private sessionId = "";

  setSessionId(id: string): void {
    this.sessionId = id;
  }

  push(text: string): void {
    const msg: SDKUserMessage = {
      type: "user",
      message: { role: "user", content: text },
      parent_tool_use_id: null,
      session_id: this.sessionId,
    };
    const waiter = this.waiters.shift();
    if (waiter) waiter(msg);
    else this.queue.push(msg);
  }

  async *stream(): AsyncIterable<SDKUserMessage> {
    for (;;) {
      const next = this.queue.shift();
      if (next) {
        yield next;
        continue;
      }
      yield await new Promise<SDKUserMessage>((resolve) => this.waiters.push(resolve));
    }
  }
}

// taskPrompt used to live here. It synthesized:
//
//     You are working on task <uuid> in repo <name>.
//     Task: <description><guidance>
//
// and pushed it into the input queue before query() was even constructed.
//
// It is deleted in docs/adr/0048. There is no fleet-authored prompt at all
// now: the first message a human sends IS the first user turn, verbatim,
// exactly as `claude "fix the flaky test"` behaves. The session's instruction
// arrives through the same streamHumanMessages feed as every later message,
// so there is no first-turn branch anywhere in this file.
//
// That also removes the wrapper's two quiet failure modes. `guidance` was
// always empty in practice — the sidecar hardcoded it — so the operator's
// chosen prompt snippets never actually reached the model; they now prefill
// the dashboard's composer, where a human can see and edit them before
// sending. And a description the agent could not see was a description that
// silently drifted from what it had really been asked to do.

// An SDK message's own fields minus the envelope the transcript row
// already carries, with long strings capped so a chatty hook's stdout
// can't dominate the entry (same 2000-char cap the tool_result branch
// uses). parent_tool_use_id deliberately survives: it is the stream's only
// subagent attribution, and unlike uuid/session_id the transcript row
// carries no equivalent of it.
function relayFields(msg: { [key: string]: unknown }): Record<string, unknown> {
  const envelope = new Set(["type", "subtype", "uuid", "session_id"]);
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(msg)) {
    if (envelope.has(key)) continue;
    out[key] = typeof value === "string" && value.length > 2000 ? `${value.slice(0, 2000)}…` : value;
  }
  return out;
}

// Relays every SDK message, tagged by its own raw type, to the transcript
// (reliability-findings.md #0: "relay everything, let the UI decide, no
// pre-filtering" — before this, only assistant text blocks ever left this
// function). Assistant *text* keeps type "discussion" specifically, same
// as before, since that's what core's relay allowlist treats as
// Discord-safe narrative; everything else (tool_use input, tool_result
// content, session-init metadata, result summaries) is tagged with its
// own SDK message type, which the relay's allowlist does NOT forward to
// Discord — dashboard-only visibility, no secret-leak risk. The transcript
// row itself is unfiltered either way (core/internal/transcript/relay.go's
// own comment: only relayed_to_discord changes, dashboard renders the
// full stream regardless).
async function logSdkMessage(actor: string, msg: { type: string; [key: string]: unknown }): Promise<void> {
  const push = (text: string, type: string) =>
    sidecar.pushMessage(actor, text, type).catch((err) => log("warn", "pushMessage failed", { actor, type, error: String(err) }));

  if (msg.type === "system" && msg.subtype === "init") {
    log("info", `${actor} session started`, {
      model: msg.model,
      mcpServers: msg.mcp_servers,
      permissionMode: msg.permissionMode,
      skills: msg.skills,
      plugins: msg.plugins,
    });
    await push(
      JSON.stringify({
        model: msg.model,
        permissionMode: msg.permissionMode,
        slashCommands: msg.slash_commands,
        skills: msg.skills,
        tools: msg.tools,
        mcpServers: msg.mcp_servers,
        cwd: msg.cwd,
        claudeCodeVersion: msg.claude_code_version,
        agents: msg.agents,
        plugins: msg.plugins,
        outputStyle: msg.output_style,
      }),
      "system",
    );
    return;
  }
  // Out-of-band session signals — every non-init system subtype (status,
  // compact_boundary, hook_response), plus auth_status and tool_progress.
  // Before this they hit none of the branches below and were dropped
  // silently, which is why an expired OAuth token, a rate limit, and a
  // four-minute Bash call all looked identical from the dashboard: a
  // worker that had simply stopped talking.
  //
  // All relay under the existing "system" type, so this needs no migration
  // (db/migrations/000004's CHECK already allows it) and stays
  // dashboard-only for free (core/internal/transcript/relay.go's Discord
  // allowlist fails closed). `sdk` is the discriminant the UI branches on,
  // and the SDK's own field names pass through unmapped so a subtype the
  // SDK adds next relays itself without a worker change.
  //
  // tool_progress needs no throttling here: the SDK emits at most one per
  // 30s (verified live — a 100s Bash produced exactly three), and it only
  // emits them at all when CLAUDE_CODE_CONTAINER_ID is set, which the
  // provisioner now does. Its `tool_use_id` is synthetic and unique per
  // emission ("bash-progress-30", "-60", …), NOT the originating
  // toolu_… id, so nothing downstream should try to pair on it.
  if (msg.type === "system" || msg.type === "auth_status" || msg.type === "tool_progress") {
    // compact_boundary is the only one of these that costs something: the
    // session just lost its detailed history to a summary. It carries the
    // pre-compaction token count, which is the single most useful context
    // metric the SDK emits — surface it at warn so it's greppable in Loki
    // rather than buried in a system entry nothing reads (ADR-0046).
    const compactPreTokens = (msg.compact_metadata as { pre_tokens?: number } | undefined)?.pre_tokens;
    log(compactPreTokens === undefined ? "info" : "warn", `${actor} ${msg.subtype ?? msg.type}`, {
      ...relayFields(msg),
      ...(compactPreTokens === undefined ? {} : { preTokens: compactPreTokens }),
    });
    await push(JSON.stringify({ sdk: msg.subtype ?? msg.type, ...relayFields(msg) }), "system");
    return;
  }
  if (msg.type === "assistant") {
    // An SDK-level assistant error (rate_limit, billing_error,
    // authentication_failed, server_error) arrives on the message itself,
    // not as a content block — the turn just comes back empty otherwise.
    if (msg.error) {
      log("error", `${actor} assistant error`, { error: msg.error });
      await push(JSON.stringify({ sdk: "assistant_error", error: msg.error }), "system");
    }
    const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
    for (const block of content) {
      if (block.type === "text" && typeof block.text === "string" && block.text.trim()) {
        log("info", `${actor} text`, { text: block.text });
        await push(block.text, "discussion");
      }
      // Thinking carries its prose on `thinking`, not `text`, and shares
      // the "assistant" type with tool_use — `kind` keeps the two apart
      // for a parser that already expects {id, tool, input} there.
      if (block.type === "thinking" && typeof block.thinking === "string" && block.thinking.trim()) {
        await push(JSON.stringify({ kind: "thinking", text: block.thinking }), "assistant");
      }
      if (block.type === "tool_use") {
        log("info", `${actor} tool_use`, { tool: block.name, input: block.input });
        await push(JSON.stringify({ id: block.id, tool: block.name, input: block.input }), "assistant");
      }
    }
    return;
  }
  if (msg.type === "user") {
    // The SDK's own "this message is already in the messages array"
    // marker. Relaying it posts the entry a second time.
    if (msg.isReplay) return;
    const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
    for (const block of content) {
      if (block.type === "tool_result") {
        const isError = Boolean(block.is_error);
        const resultContent = typeof block.content === "string" ? block.content.slice(0, 2000) : block.content;
        log(isError ? "error" : "info", `${actor} tool_result`, { isError, content: resultContent });
        // toolUseId correlates back to the tool_use block's own `id` above —
        // the dashboard pairs call+output into one collapsible unit instead
        // of two unrelated-looking bubbles (standard Anthropic content-block
        // shape: tool_result always carries the originating tool_use's id).
        await push(JSON.stringify({ toolUseId: block.tool_use_id, isError, content: resultContent }), "user");
      }
    }
    return;
  }
  if (msg.type === "result") {
    // Token fields ride the log line, not just the transcript row: `usage`
    // has always been stored at :usage below, but only the transcript saw
    // it, so the fleet had no queryable context metric at all. inputTokens
    // is the number the context budget is judged on — a session that climbs
    // monotonically toward the compact threshold shows up here first
    // (ADR-0046). cacheReadInputTokens separates "context is large" from
    // "context is large AND being re-read uncached", which cost differently.
    const usage = msg.usage as
      | { input_tokens?: number; output_tokens?: number; cache_read_input_tokens?: number }
      | undefined;
    log("info", `${actor} result`, {
      subtype: msg.subtype,
      numTurns: msg.num_turns,
      totalCostUsd: msg.total_cost_usd,
      inputTokens: usage?.input_tokens,
      outputTokens: usage?.output_tokens,
      cacheReadInputTokens: usage?.cache_read_input_tokens,
    });
    await push(
      JSON.stringify({
        subtype: msg.subtype,
        numTurns: msg.num_turns,
        totalCostUsd: msg.total_cost_usd,
        durationMs: msg.duration_ms,
        durationApiMs: msg.duration_api_ms,
        isError: msg.is_error,
        usage: msg.usage,
        modelUsage: msg.modelUsage,
        permissionDenials: msg.permission_denials,
        errors: msg.errors,
      }),
      "result",
    );
  }
  // ponytail: `user` text/image content blocks and `stream_event` stay
  // unrelayed. The former is the echo of a prompt the dashboard already
  // posted as a discussion entry; the latter is per-token and belongs on a
  // live-only channel, not in a durable Postgres transcript.
}

export type SessionResult = { aborted: boolean; summary: string };

// runTask is the continuous-session driver (docs/adr/0021, reliability-
// findings.md #0): one query() call, streaming-input mode, spans planning
// and implementation with no fleet-imposed phase boundary or teardown/
// restart at the approval boundary.
export async function runSession(): Promise<SessionResult> {
  // The session row, read fresh at startup rather than trusted from env.
  //
  // This is the whole reason GET /task survives docs/adr/0048's cull of the
  // sidecar's wrapper-facing API: permission_mode has to be RESTORED on a
  // warm. Without it, every resume of a session a human put into
  // acceptEdits/plan/bypassPermissions would silently revert to "default",
  // and the human would have no way to tell except by watching it start
  // asking again.
  const session = await sidecar.getSession();

  let aborted = false;
  // Set by an "interrupt" entry (DashboardService.Interrupt) — stops only
  // the current turn via q.interrupt(), unlike "abort" which ends the
  // whole session. Consumed by the very next `result` message so that
  // turn's non-"success" subtype is treated as an expected soft-stop, not
  // a real failure, without the session ending.
  // ponytail: a soft-interrupt clicked while genuinely idle (no active
  // turn to interrupt) is a no-op that leaves this flag stale until
  // whatever `result` comes next — harmless in the common case (that
  // result is a real success either way) but would incorrectly swallow a
  // genuine error on the very next round. Add explicit turn-active
  // tracking if this idle-click race ever actually bites.
  let softInterrupted = false;
  let finalText = "";

  // Generalizes ExitPlanMode's old bespoke pendingPlanDecision var to every
  // tool call: canUseTool posts a permission_request transcript entry and
  // parks its resolver here, keyed by that entry's own seq, exactly the
  // same "ask, block, wait for an async human decision" shape the old
  // ExitPlanMode-only gate used — ExitPlanMode is no longer special-cased,
  // it's just one more entry in this map (docs/adr supersession of
  // 0021/0025's Write/Edit-absent-from-allowedTools gate).
  const pendingPermissions = new Map<number, { resolve: (result: PermissionResult) => void; input: Record<string, unknown> }>();

  function resolvePending(seq: number, build: (input: Record<string, unknown>) => PermissionResult): boolean {
    const pending = pendingPermissions.get(seq);
    if (!pending) return false;
    pendingPermissions.delete(seq);
    pending.resolve(build(pending.input));
    return true;
  }

  // Records a decision this wrapper made on the human's behalf, so it exists
  // somewhere other than this process's memory.
  //
  // Everything downstream — the dashboard's feed card, its dock, its list
  // row — derives "is this still waiting on a human" from the transcript
  // alone: a PERMISSION_RESPONSE replying to the request's seq, or a later
  // INTERRUPT/ABORT. The two sweeps below resolved the SDK's blocked
  // canUseTool promise and wrote nothing, so a plan answered by requesting
  // changes stayed pending forever on every one of those surfaces — the big
  // plan card could only ever be dismissed by approving it.
  //
  // from "agent", not "human": the human typed feedback, not a structured
  // decision, and core streams from=="human" entries back into this very
  // session as new input (StreamHumanMessages' own filter). Writing these as
  // human would feed the wrapper its own bookkeeping.
  //
  // Fire-and-forget on purpose. The callers resolve a blocked canUseTool and
  // must not await between asking and being ready for the answer; a failed
  // write is worth a log line, never a stalled permission.
  function recordResolution(seq: number, decision: { behavior: "allow" | "deny"; message?: string }): void {
    void sidecar
      .pushMessage("agent", JSON.stringify(decision), "permission_response", seq)
      .catch((err) => log("warn", "recording auto-resolved permission failed", { taskId: SESSION_ID, seq, error: String(err) }));
  }

  // A permission-mode switch (the dashboard's mode picker) pre-empts
  // whatever's currently pending — mirrors Claude Code CLI's own
  // ExitPlanMode prompt, where picking "yes, and don't ask again for
  // edits" both answers the pending call and changes the mode in one move.
  function resolveAllPendingAllow(): void {
    for (const [seq, pending] of pendingPermissions) {
      pending.resolve({ behavior: "allow", updatedInput: pending.input });
      recordResolution(seq, { behavior: "allow", message: "Allowed by a permission-mode change." });
    }
    pendingPermissions.clear();
  }

  function resolveAllPendingDeny(message: string, interrupt = false): void {
    for (const [seq, pending] of pendingPermissions) {
      pending.resolve({ behavior: "deny", message, ...(interrupt ? { interrupt: true } : {}) });
      // An interrupt/abort already leaves its own entry, and the dashboard
      // reads that as resolving everything pending at that moment — writing
      // a second record for the same event would be redundant, not clearer.
      if (!interrupt) recordResolution(seq, { behavior: "deny", message });
    }
    pendingPermissions.clear();
  }

  const input = new InputQueue();
  // Deliberately NOT seeded. The queue is fed exclusively by
  // streamHumanMessages below, so the session's first message reaches the
  // model as an ordinary human turn rather than a fleet-authored wrapper
  // (docs/adr/0048).
  //
  // This is why core must warm the pod BEFORE appending that first message:
  // RESUME_FROM_SEQ is computed at provisioning time, so a message appended
  // first would land below this pod's cursor and never arrive — the pod
  // would boot, find nothing to do, and be swept as stalled.

  const abortController = new AbortController();
  const q = query({
    prompt: input.stream(),
    options: {
      executable: "bun",
      cwd: WORKTREE_PATH,
      // Use the task's model from the database, falling back to global env
      // var or default if not set. This allows per-task model selection.
      model: session.model ?? MODEL,
      // Continues the prior conversation instead of starting fresh when
      // this pod is warming an existing session (Phase 4's Warm action) —
      // relies on CLAUDE_CONFIG_DIR already pointing at the shared PVC and
      // `cwd` matching exactly what it was last time (same worktree path,
      // guaranteed since it's always /workspace/worktrees/<taskId>).
      resume: RESUME_SESSION_ID,
      // Without this, a crash only ever surfaces as the SDK's generic
      // "Claude Code process exited with code 1" (worker/src/index.ts's
      // catch) — the child process's actual stderr (auth failure, bad
      // resume session, etc.) is otherwise discarded, not just unlogged.
      stderr: (data) => log("error", "claude stderr", { taskId: SESSION_ID, data }),
      // Use the task's permission mode from the database, falling back to
      // "default" if not set. This ensures the mode is restored on resume
      // instead of always resetting to "default".
      permissionMode: (session.permissionMode ?? "default") as PermissionMode,
      // Write/Edit/Bash are deliberately never in this list — canUseTool
      // below is the live escalation path for all three (confirmed in
      // Phase 0's spike: allowedTools bypasses canUseTool entirely for
      // anything it lists, so declaring them here would silently remove
      // the gate, not add one).
      // This list is load-bearing and stays (docs/adr/0048). Deleting it was
      // proposed on the grounds that "default" mode already auto-allows
      // reads and prompts on writes — which is false for MCP tools. Checked
      // against the vendored SDK: every MCP tool's checkPermissions returns
      // {behavior: "passthrough"}, and the evaluator converts passthrough to
      // "ask". WebSearch is passthrough too. With no allowlist, the agent
      // would need a human's permission to call send_message, and would need
      // permission before it could ASK for permission.
      //
      // The nine explicit mcp__agent-fleet-sidecar__<name> entries that used
      // to sit here ARE gone: the wildcard beside them already matched every
      // one (the SDK's rule matcher treats an mcp rule with toolName "*" or
      // undefined as matching the whole server), so they were a second,
      // hand-maintained copy of the same policy that every new tool had to
      // be added to.
      allowedTools: [
        "Read", "Glob", "Grep", "WebSearch", "WebFetch", "Task",
        "mcp__agent-fleet-sidecar__*",
        // Playwright by the same wildcard rule. Every browser call would
        // otherwise prompt — including the read-only ones a human would never
        // want to be asked about (snapshot, console messages) — because MCP
        // tools are passthrough → ask, not because navigating is dangerous.
        //
        // The browser only ever reaches what this pod can reach, and the pod's
        // own network position is the real boundary. What it can DO with a
        // page is bounded by the same thing bounding the agent generally.
        "mcp__playwright__*",
      ],
      // The SDK's own built-in "AskUserQuestion" (distinct from the
      // mcp__agent-fleet-sidecar__ one above) is available by default and
      // has the same name/shape docs/adr/0018 deliberately mirrored — the
      // model reaches for whichever it sees first, and the native one
      // falls through canUseTool into the generic PermissionCard (raw
      // JSON + Allow/Deny) instead of the dashboard's QUESTION form, with
      // no way to actually deliver a chosen answer back. Removing it from
      // context forces the only question tool that's actually wired up.
      disallowedTools: ["AskUserQuestion"],
      // No tool classification here at all — the SDK's own permission mode
      // already decides when canUseTool gets invoked (bypassPermissions
      // skips it entirely, acceptEdits skips it for file edits, plan blocks
      // mutation without invoking it, default invokes it for anything not
      // auto-safe). This callback's only job is "ask a human and block" —
      // reproducing exactly what happens interactively in the CLI, not a
      // second fleet-side gate on top of it (supersedes docs/adr/0021's
      // Write/Edit-absent-from-allowedTools framing and adr/0025's
      // approve-signal mechanism).
      canUseTool: async (toolName, toolInput) => {
        // The human is shown (and answers) the command the agent actually
        // wrote — what gets parked below, and so what every resolve path
        // hands back as updatedInput, is its rtk-compacted equivalent. Same
        // command, a fraction of the output. Doing it here rather than in a
        // PreToolUse hook is what keeps the gate: a hook can only rewrite a
        // call it also allows outright (see rtkHook.ts). Modes that skip
        // canUseTool entirely (bypassPermissions/acceptEdits) skip the
        // rewrite too — no gate to preserve there, just no rtk either.
        //
        // Ahead of the push, not between it and the pendingPermissions.set:
        // spawning rtk takes tens of milliseconds, and a decision that
        // arrives inside that window has no resolver to find, leaving the
        // tool call blocked forever on a permission a human already
        // answered. Nothing may await between asking and being ready for
        // the answer.
        const effectiveInput = await rtkRewrite(toolName, toolInput as Record<string, unknown>);
        const seq = await sidecar
          .pushMessage("agent", JSON.stringify({ tool: toolName, input: toolInput }), "permission_request")
          .catch((err) => {
            log("warn", "permission_request push failed", { toolName, error: String(err) });
            return null;
          });
        if (seq === null) {
          return { behavior: "deny", message: "Could not reach the dashboard to request permission — try again." };
        }
        return new Promise<PermissionResult>((resolve) => {
          pendingPermissions.set(seq, { resolve, input: effectiveInput });
        });
      },
      mcpServers: {
        "agent-fleet-sidecar": sidecarMcpServer(),
        playwright: playwrightMcpServer(),
      },
      // "user": CLAUDE_CONFIG_DIR (provisioner/internal/k8s/pod.go) points
      // at the shared PVC, provisioner-synced by git.Manager.SyncFleetShared
      // (docs/adr/0032) — Claude Code discovers CLAUDE.md/settings.json/
      // skills/ there natively, no plugins: entry needed.
      //
      // "project" (docs/adr/0049, resolving adr/0032's deferred question):
      // the target repo's own CLAUDE.md and .claude/skills/, relative to cwd
      // = WORKTREE_PATH. Without it a worker on agent-fleet never read
      // agent-fleet's own CLAUDE.md — it was working from the fleet-shared
      // one alone.
      //
      // It also merges the target repo's .claude/settings.json
      // permissions.allow, and an allow rule removes canUseTool from the
      // path entirely (same authority as allowedTools above). What stops a
      // repo self-approving its own `gh api` is the permissions.ask block in
      // fleet-shared/settings.json: the SDK's evaluator resolves
      // deny → ask → allow and returns on the first match, so a user-scope
      // ask outranks a project-scope allow. Widening that block is the same
      // decision as adding to allowedTools.
      //
      // Deliberately NOT "local": .claude/settings.local.json is gitignored,
      // so nothing about it is reviewable in the PR that lands it.
      settingSources: ["user", "project"],
      maxTurns: MAX_TURNS,
      abortController,
    },
  });

  // q.interrupt() is not guaranteed to settle, so it is never awaited bare.
  //
  // The SDK registers a control request and resolves it when the CLI answers.
  // Its cleanup() clears pendingControlResponses WITHOUT rejecting them, and
  // runs whenever the CLI message stream ends — so a request issued after the
  // CLI has died waits on a promise nothing will ever settle, and not even the
  // .catch() can fire. That parks this callback inside the SSE stream, which
  // parks streamHumanMessages, which parks runSession's finally, which means
  // index.ts never reaches process.exit: the Job never goes terminal and the
  // concurrency slot leaks. Precisely the failure class the explicit exit was
  // added to remove — reachable by "the CLI dies, then a human clicks Stop".
  //
  // 5s is far beyond a healthy round-trip; if it elapses, the channel is gone
  // and abortController.abort() is what actually ends the session anyway.
  const interruptSafely = async (what: string) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      q.interrupt().catch((err) => log("warn", `${what} failed`, { taskId: SESSION_ID, error: String(err) })),
      new Promise<void>((resolve) => {
        timer = setTimeout(() => {
          log("warn", `${what} did not settle — the SDK control channel is gone`, { taskId: SESSION_ID });
          resolve();
        }, 5_000);
      }),
    ]);
    clearTimeout(timer);
  };

  // Delivers every new human message live (docs/adr/0021 point 2) — not a
  // poll the wrapper initiates on its own schedule.
  const humanMessagesAbort = new AbortController();
  const humanMessagesDone = sidecar
    .streamHumanMessages(async (entry) => {
      // "session" is another session's prompt, relayed by core
      // (docs/adr/0041). Our own output is from="agent" and must never be
      // fed back in — that is what this filter is for.
      if (entry.from !== "human" && entry.from !== "session") return;
      // An "answer" to an AskUserQuestion. While a pod is live and its
      // AskUserQuestion tool call is mid-flight, core's own poll loop
      // consumes the answer and returns it as the tool_result — nothing to
      // do here. But a blocking question can outlive its pod: the human
      // answers days later, after the pod was idle-torn-down, so there is
      // no live tool call and no core poll waiting. On the next warm this
      // is the only path that delivers the choice — replayed above
      // RESUME_FROM_SEQ like any other human entry — so feed it in as an
      // ordinary turn ("the human answered your earlier question: ...")
      // and let the agent continue. Pushes a labeled turn (not the raw
      // answer JSON that input.push at the end would post) and returns.
      // ponytail: a live blocking poll that IS mid-flight when the answer
      // lands gets it both ways (tool_result AND this turn) — a harmless
      // redundant turn the agent has already acted on; dedup by replyTo if
      // it ever actually bites.
      if (entry.type === "answer") {
        input.push(`Answer to your earlier question (seq ${entry.replyTo ?? "?"}): ${entry.text}`);
        return;
      }
      if (entry.type === "abort") {
        aborted = true;
        // Nothing pending has anything left to wait for — deny it all so
        // no promise dangles past the session teardown below.
        resolveAllPendingDeny("Mohammad stopped the session.", true);
        // interrupt() is graceful but only meaningful mid-turn — if the
        // session is idle (paused between rounds, waiting for the next
        // streamed input, the common case for an unprompted /stop),
        // there's nothing active for it to interrupt and the `for await`
        // loop below would otherwise hang forever waiting for a message
        // that will never come. abortController.abort() guarantees the
        // underlying session actually ends either way.
        await interruptSafely("interrupt");
        abortController.abort();
        return;
      }
      // A soft interrupt (DashboardService.Interrupt) — stops only the
      // current turn, session/pod stay alive. Denying any pending
      // permission with interrupt:true is the SDK's own mid-permission-
      // prompt stop signal (the model can't be interrupted out of a
      // canUseTool call any other way, since that call is blocked on our
      // own Promise, not the SDK's internal generation loop); q.interrupt()
      // additionally covers the plain-generation/already-approved-tool
      // case where nothing is pending.
      if (entry.type === "interrupt") {
        softInterrupted = true;
        if (pendingPermissions.size > 0) {
          resolveAllPendingDeny(entry.text || "Mohammad interrupted the current turn.", true);
        }
        await interruptSafely("soft interrupt");
        return;
      }
      // The human's allow/deny decision for a specific permission_request,
      // correlated by seq — DashboardService.RespondToPermission (a
      // generalization of AskUserQuestion's own ask-and-answer plumbing).
      if (entry.type === "permission_response") {
        if (typeof entry.replyTo !== "number") return;
        let decision: { behavior?: "allow" | "deny"; updatedInput?: Record<string, unknown>; message?: string } = {};
        try {
          decision = JSON.parse(entry.text);
        } catch {
          return;
        }
        resolvePending(entry.replyTo, (input) =>
          decision.behavior === "allow"
            ? { behavior: "allow", updatedInput: decision.updatedInput ?? input }
            : { behavior: "deny", message: decision.message ?? "Denied." },
        );
        return;
      }
      // The dashboard's permission-mode selector (docs/adr/0027) — a mode
      // switch pre-empts whatever's currently pending (see
      // resolveAllPendingAllow's own comment) rather than leaving it to
      // time out unanswered.
      if (entry.type === "permission_mode") {
        const mode = entry.text as PermissionMode;
        // Caught, not bare-awaited: this is the only await in this callback
        // that could reject, and a rejection here does not just lose the mode
        // switch — it escapes onEntry, streamOnce rethrows, and nextSeq is
        // only advanced AFTER onEntry resolves. The reconnect would redeliver
        // the same entry and throw again every 2s, permanently, so no later
        // reply, permission response or Stop would ever be delivered and
        // anything already pending would block forever. Query.request()
        // rejects on any non-success control response, which an unrecognized
        // mode string is.
        await q
          .setPermissionMode(mode)
          .catch((err) => log("warn", "setPermissionMode failed", { taskId: SESSION_ID, mode, error: String(err) }));
        resolveAllPendingAllow();
        return;
      }
      // `/clear` (and its CLI aliases) must NOT reach the SDK. In
      // streaming-input mode the reader throws
      //   "only prompt commands are supported in streaming mode"
      // for any command whose type isn't "prompt", and `clear` is a `local`
      // command (verified in the vendored cli.js: `name:"clear", …,
      // supportsNonInteractive:false`). Pushing it as a user turn crashes
      // the whole query. The SDK's Query control surface has no
      // conversation-reset request either (runtimeTypes.d.ts) — the
      // interactive `/clear` only wipes the REPL's in-memory state, which is
      // unreachable from here. So the only "clear" streaming mode allows is
      // a *fresh session*: drop the saved resume id and end this pod. The
      // next Warm boots with no `resume`, i.e. an empty conversation. Same
      // teardown path as abort (interrupt the current turn, then
      // abortController.abort() so an idle between-rounds session doesn't
      // hang), but preceded by clearing the resume identity.
      if (/^\/(clear|reset|new)\s*$/.test(entry.text.trim())) {
        aborted = true;
        resolveAllPendingDeny("Conversation cleared.", true);
        // Empty id => next warm passes resume: undefined => fresh session.
        await sidecar
          .saveSessionId("", session.model ?? MODEL)
          .catch((err) => log("warn", "saveSessionId(clear) failed", { taskId: SESSION_ID, error: String(err) }));
        await sidecar
          .pushMessage("agent", "Conversation cleared — re-warm this session for a fresh start.", "system")
          .catch(() => {});
        await interruptSafely("clear");
        abortController.abort();
        return;
      }
      if (pendingPermissions.size > 0) {
        // A plain reply while something's pending is feedback, not an
        // answer — deny every pending request with it (same as choosing
        // "No, keep planning" with guidance in Claude Code CLI's own
        // ExitPlanMode prompt: interrupt stays unset so the model
        // incorporates the reply and keeps going, then retries the call)
        // AND push it as a real conversational turn below — a denial
        // message embedded in a tool_result reads as "my call failed," not
        // "Mohammad is talking to me right now." Without falling through,
        // the only trace of his reply was buried once per pending call.
        resolveAllPendingDeny(`Mohammad replied (not an approval): ${entry.text}`);
      }
      input.push(entry.text);
    }, humanMessagesAbort.signal, RESUME_FROM_SEQ)
    .catch((err) => log("error", "human message stream failed", { taskId: SESSION_ID, error: String(err) }));

  let sessionId = "";

  async function driveQuery(): Promise<void> {
    for await (const msg of q) {
      if (!sessionId && "session_id" in msg) {
        sessionId = msg.session_id as string;
        // Identity re-check (docs/adr/0041). We asked the SDK to resume a
        // specific session; if what came back is a different id, the resume
        // did not happen and this agent is starting from nothing — no prior
        // context, no history — while every outside signal (task row, pod,
        // transcript) still says "resumed". Overwriting tasks.session_id
        // silently, as this used to, made the two cases identical.
        //
        // The new session is still saved and the run continues: a fresh
        // session that works beats refusing to run. But it says so, loudly
        // and durably, so a session that quietly forgot everything is
        // visible rather than merely confusing.
        if (RESUME_SESSION_ID && RESUME_SESSION_ID !== sessionId) {
          log("error", "resume failed: SDK returned a different session id", {
            taskId: SESSION_ID,
            requested: RESUME_SESSION_ID,
            got: sessionId,
          });
          await sidecar
            .pushMessage(
              "agent",
              JSON.stringify({ sdk: "resume_failed", requested: RESUME_SESSION_ID, got: sessionId }),
              "system",
            )
            .catch(() => {});
        }
        input.setSessionId(sessionId);
        // Save the session ID and model to the database
        const modelToSave = session.model ?? MODEL;
        await sidecar
          .saveSessionId(sessionId, modelToSave)
          .catch((err) => log("warn", "saveSessionId failed", { taskId: SESSION_ID, error: String(err) }));
        // Also persist the initial permission mode if not already set. This
        // ensures the mode shows up in the dashboard instead of "?".
        if (!session.permissionMode) {
          await sidecar
            .savePermissionMode("default")
            .catch((err) => log("warn", "savePermissionMode failed", { taskId: SESSION_ID, error: String(err) }));
        }
      }
      await logSdkMessage("agent", msg as { type: string; [key: string]: unknown });

      if (msg.type === "assistant") {
        const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
        for (const block of content) {
          if (block.type === "text") finalText = block.text as string;
        }
      }

      if (msg.type === "result") {
        if (aborted) break;
        if (softInterrupted) {
          softInterrupted = false;
          continue;
        }
        // Uniform non-success handling (reliability-findings.md #8) — no
        // more approved/!approved branch split, since there's no longer a
        // fleet-imposed phase boundary to split on. A non-success result
        // at any point in the session is treated the same way.
        if (msg.subtype === "success") {
          // No automated completion detection at all (no PR_READY: text
          // match, no PR-existence check) — a session only ever ends via
          // aborted above (the human's own Stop) or a genuinely thrown
          // error below. A success result just means this round is done;
          // the session pauses naturally, waiting for the next streamed
          // input, exactly like an idle terminal `claude` session.
          continue;
        }
        // The 0-turns/$0 "transient" classification used to split this into
        // two error types so core's reclaim could re-dispatch one of them.
        // Nothing reclaims a session now, so both outcomes are identical:
        // the pod exits non-zero and a human sees why. The turn/cost figures
        // stay in the message, since they are what distinguishes "the SDK
        // never got started" from "it ran and failed".
        throw new Error(`session stopped: ${msg.subtype} after ${msg.num_turns} turns, $${msg.total_cost_usd}`);
      }
    }
  }

  try {
    await driveQuery();
  } catch (err) {
    // An aborted session throws (real SDK: AbortError; the fake mock
    // mirrors this) — expected once `aborted` is already set, not a real
    // failure to propagate.
    if (!aborted) throw err;
  } finally {
    humanMessagesAbort.abort();
    abortController.abort();
    await humanMessagesDone;
  }

  return { aborted, summary: finalText };
}
