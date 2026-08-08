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

// Absolute in-container path baked at image build time — the local skills
// plugin (doubt-driven-development, architecture-interview), bundled the
// same way as before this rewrite (see docs/adr/0017). Must be absolute:
// the session's cwd is the target repo's worktree, not this one.
const PLANNING_SKILLS_PLUGIN_PATH = "/app/worker/skills/agent-fleet-planning";

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
      }),
      "system",
    );
    return;
  }
  if (msg.type === "assistant") {
    const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
    for (const block of content) {
      if (block.type === "text" && typeof block.text === "string" && block.text.trim()) {
        log("info", `${actor} text`, { text: block.text });
        await push(block.text, "discussion");
      }
      if (block.type === "tool_use") {
        log("info", `${actor} tool_use`, { tool: block.name, input: block.input });
        await push(JSON.stringify({ id: block.id, tool: block.name, input: block.input }), "assistant");
      }
    }
    return;
  }
  if (msg.type === "user") {
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
    await push(JSON.stringify({ subtype: msg.subtype, numTurns: msg.num_turns, totalCostUsd: msg.total_cost_usd }), "result");
  }
}

export type TaskResult = { aborted: boolean; summary: string };

// runTask is the continuous-session driver (docs/adr/0021, reliability-
// findings.md #0): one query() call, streaming-input mode, spans planning
// and implementation with no fleet-imposed phase boundary or teardown/
// restart at the approval boundary.
export async function runTask(task: Task): Promise<TaskResult> {
  let aborted = false;
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
      model: MODEL,
      // Continues the prior conversation instead of starting fresh when
      // this pod is warming an existing session (Phase 4's Warm action) —
      // relies on CLAUDE_CONFIG_DIR already pointing at the shared PVC and
      // `cwd` matching exactly what it was last time (same worktree path,
      // guaranteed since it's always /workspace/worktrees/<taskId>).
      resume: RESUME_SESSION_ID,
      // "default" — matches running `claude` locally with no flags. The
      // dashboard's mode picker can switch to plan/acceptEdits/
      // bypassPermissions at any point (docs/adr/0027's SetPermissionMode,
      // reused verbatim); there's no fleet-imposed starting gate anymore.
      permissionMode: "default",
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
        "mcp__agent-fleet-sidecar__*",
      ],
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
      plugins: [{ type: "local", path: PLANNING_SKILLS_PLUGIN_PATH }],
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
        await sidecar
          .saveSessionId(sessionId, MODEL)
          .catch((err) => log("warn", "saveSessionId failed", { taskId: task.id, error: String(err) }));
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
