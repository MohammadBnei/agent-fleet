import { query, type SDKUserMessage, type PermissionResult, type PermissionMode } from "@anthropic-ai/claude-agent-sdk";
import type { Task } from "./types.js";
import * as sidecar from "./sidecarClient.js";
import { log } from "./log.js";

// Thrown when an SDK result looks like a transient infra/SDK hiccup (0
// turns, $0 cost) rather than a genuine failure — the caller leaves it for
// core's own reclaim instead of failing the task outright. See docs/adr/0016.
export class TransientError extends Error {}

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

// This is deliberately thin: "Claude Code, reached through a dashboard" —
// the fleet's value is the infra (pod, worktree, dashboard access), not a
// bespoke conversation script layered on top (supersedes docs/adr/0021/
// 0025's phase-boundary framing and the EXPLORE/REVIEW/INTERVIEW/PLAN/
// DOUBT/RECONCILE workflow this function used to force on every task
// unconditionally). The agent discovers its own worktree/branch state via
// git, and its own tools via the MCP tool list, the same way a human
// resuming work in a terminal would — nothing here needs to say it. Any
// process guidance (architecture-interview, doubt-driven-development,
// posting a plan, opening a PR) is opt-in per task, via task.guidance —
// the operator's chosen prompt_snippets (dashboard-editable, see
// db/schema.sql), joined once at task-creation time. "" attaches nothing.
function taskPrompt(task: Task): string {
  const guidance = task.guidance ? `\n\n${task.guidance}` : "";
  return `You are working on task ${task.id} in repo ${task.repo}.
Task: ${task.description}${guidance}`;
}

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
    log("info", `${actor} ${msg.subtype ?? msg.type}`, relayFields(msg));
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
    log("info", `${actor} result`, { subtype: msg.subtype, numTurns: msg.num_turns, totalCostUsd: msg.total_cost_usd });
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

export type TaskResult = { aborted: boolean; summary: string };

// runTask is the continuous-session driver (docs/adr/0021, reliability-
// findings.md #0): one query() call, streaming-input mode, spans planning
// and implementation with no fleet-imposed phase boundary or teardown/
// restart at the approval boundary.
export async function runTask(task: Task): Promise<TaskResult> {
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

  // A permission-mode switch (the dashboard's mode picker) pre-empts
  // whatever's currently pending — mirrors Claude Code CLI's own
  // ExitPlanMode prompt, where picking "yes, and don't ask again for
  // edits" both answers the pending call and changes the mode in one move.
  function resolveAllPendingAllow(): void {
    for (const pending of pendingPermissions.values()) pending.resolve({ behavior: "allow", updatedInput: pending.input });
    pendingPermissions.clear();
  }

  function resolveAllPendingDeny(message: string, interrupt = false): void {
    for (const pending of pendingPermissions.values()) pending.resolve({ behavior: "deny", message, ...(interrupt ? { interrupt: true } : {}) });
    pendingPermissions.clear();
  }

  const input = new InputQueue();
  input.push(taskPrompt(task));

  const abortController = new AbortController();
  const q = query({
    prompt: input.stream(),
    options: {
      executable: "bun",
      cwd: WORKTREE_PATH,
      // Use the task's model from the database, falling back to global env
      // var or default if not set. This allows per-task model selection.
      model: task.model ?? MODEL,
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
      stderr: (data) => log("error", "claude stderr", { taskId: task.id, data }),
      // Use the task's permission mode from the database, falling back to
      // "default" if not set. This ensures the mode is restored on resume
      // instead of always resetting to "default".
      permissionMode: (task.permissionMode ?? "default") as PermissionMode,
      // Write/Edit/Bash are deliberately never in this list — canUseTool
      // below is the live escalation path for all three (confirmed in
      // Phase 0's spike: allowedTools bypasses canUseTool entirely for
      // anything it lists, so declaring them here would silently remove
      // the gate, not add one).
      allowedTools: [
        "Read", "Glob", "Grep", "WebSearch", "WebFetch", "Task",
        "mcp__agent-fleet-sidecar__send_message",
        "mcp__agent-fleet-sidecar__wait_for_messages",
        "mcp__agent-fleet-sidecar__AskUserQuestion",
        "mcp__agent-fleet-sidecar__request_e2e_env",
        "mcp__agent-fleet-sidecar__kill_env",
        // Named explicitly, though the wildcard below already covers it,
        // because "an un-prompted shell while Bash is gated" reads as an
        // oversight otherwise. It's deliberate (docs/adr/0039): run_command
        // lands in the e2e pod, which holds no GH_TOKEN, no
        // CLAUDE_CODE_OAUTH_TOKEN, only this task's worktree, and no git —
        // strictly less privileged than this pod, whose Bash is gated.
        // If that pod ever gains credentials or a wider mount, this entry
        // is the thing to remove.
        "mcp__agent-fleet-sidecar__run_command",
        "mcp__agent-fleet-sidecar__*",
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
          pendingPermissions.set(seq, { resolve, input: toolInput as Record<string, unknown> });
        });
      },
      mcpServers: { "agent-fleet-sidecar": sidecarMcpServer() },
      // "user" (not "project"): CLAUDE_CONFIG_DIR (provisioner/internal/k8s/
      // pod.go) points at the shared PVC, provisioner-synced by
      // git.Manager.SyncFleetShared (docs/adr/0032) — Claude Code discovers
      // CLAUDE.md/settings.json/skills/ there natively, no plugins: entry
      // needed. Deliberately not "project": that would load the *target
      // repo's own* .claude/settings.json as part of this session, a
      // separate decision not made here.
      settingSources: ["user"],
      maxTurns: MAX_TURNS,
      abortController,
    },
  });

  // Delivers every new human message live (docs/adr/0021 point 2) — not a
  // poll the wrapper initiates on its own schedule.
  const humanMessagesAbort = new AbortController();
  const humanMessagesDone = sidecar
    .streamHumanMessages(async (entry) => {
      if (entry.from !== "human") return;
      if (entry.type === "answer") return; // consumed by the blocked AskUserQuestion tool call server-side
      if (entry.type === "abort") {
        aborted = true;
        // Nothing pending has anything left to wait for — deny it all so
        // no promise dangles past the session teardown below.
        resolveAllPendingDeny("Mohammad stopped the task.", true);
        // interrupt() is graceful but only meaningful mid-turn — if the
        // session is idle (paused between rounds, waiting for the next
        // streamed input, the common case for an unprompted /stop),
        // there's nothing active for it to interrupt and the `for await`
        // loop below would otherwise hang forever waiting for a message
        // that will never come. abortController.abort() guarantees the
        // underlying session actually ends either way.
        await q.interrupt().catch((err) => log("warn", "interrupt failed", { taskId: task.id, error: String(err) }));
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
        await q.interrupt().catch((err) => log("warn", "soft interrupt failed", { taskId: task.id, error: String(err) }));
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
        await q.setPermissionMode(mode);
        resolveAllPendingAllow();
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
    .catch((err) => log("error", "human message stream failed", { taskId: task.id, error: String(err) }));

  let sessionId = "";

  async function runSession(): Promise<void> {
    for await (const msg of q) {
      if (!sessionId && "session_id" in msg) {
        sessionId = msg.session_id as string;
        input.setSessionId(sessionId);
        // Save the session ID and model to the database
        const modelToSave = task.model ?? MODEL;
        await sidecar
          .saveSessionId(sessionId, modelToSave)
          .catch((err) => log("warn", "saveSessionId failed", { taskId: task.id, error: String(err) }));
        // Also persist the initial permission mode if not already set. This
        // ensures the mode shows up in the dashboard instead of "?".
        if (!task.permissionMode) {
          await sidecar
            .savePermissionMode("default")
            .catch((err) => log("warn", "savePermissionMode failed", { taskId: task.id, error: String(err) }));
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
        const transient = msg.num_turns === 0 && msg.total_cost_usd === 0;
        const message = `session stopped: ${msg.subtype} after ${msg.num_turns} turns, $${msg.total_cost_usd}`;
        throw transient ? new TransientError(message) : new Error(message);
      }
    }
  }

  try {
    await runSession();
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
