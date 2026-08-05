import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import type { Task } from "./types.js";
import * as sidecar from "./sidecarClient.js";
import { log } from "./log.js";

// Thrown when an implementation-phase SDK result looks like a transient
// infra/SDK hiccup (0 turns, $0 cost) rather than a genuine implementation
// failure — the caller requeues instead of failing the task outright. See
// docs/adr/0016.
export class TransientError extends Error {}

const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const MAX_PLANNING_ROUNDS = Number(process.env.MAX_PLANNING_ROUNDS ?? 1);
const MAX_TURNS = process.env.MAX_TURNS ? Number(process.env.MAX_TURNS) : undefined;

const SIDECAR_MCP_ADDR = process.env.SIDECAR_MCP_ADDR ?? "localhost:9090";
const WORKTREE_PATH = process.env.WORKTREE_PATH ?? "/workspace";

// Absolute in-container path baked at image build time — the local skills
// plugin (doubt-driven-development, architecture-interview), bundled the
// same way as before this rewrite (see docs/adr/0017). Must be absolute:
// the session's cwd is the target repo's worktree, not this one.
const PLANNING_SKILLS_PLUGIN_PATH = "/app/worker/skills/agent-fleet-planning";

const PLAN_READY_PREFIX = "PLAN_READY:";

function sidecarMcpServer() {
  return { type: "http" as const, url: `http://${SIDECAR_MCP_ADDR}/mcp` };
}

// /approve and /stop are unambiguous — checked first. The word-matching
// fallback is only for anyone who types "approved"/"stop" as plain text.
function isApproval(msg: sidecar.TranscriptEntry): boolean {
  if (msg.type === "approve") return true;
  if (msg.type === "abort") return false;
  return /\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b/i.test(msg.text);
}

function isAbort(msg: sidecar.TranscriptEntry): boolean {
  if (msg.type === "abort") return true;
  if (msg.type === "approve") return false;
  return /\b(stop|abort|cancel|kill)\b/i.test(msg.text);
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

function plannerPrompt(task: Task): string {
  return `You are the PLANNER for task ${task.id} in repo ${task.repo}.
Task: ${task.description}

You are in a fresh git worktree on branch agent/${task.id}. Work through this pipeline, using your own judgment about how much of it a task this size actually needs — skip a stage and say why in one line rather than run it on autopilot:

1. EXPLORE: read the actual repository — don't guess at structure or conventions.
2. REVIEW: check your own findings for gaps before drafting anything.
3. INTERVIEW (optional): if this involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill's own DOOR classification), type "/architecture-interview" as your next message to run it. Otherwise say in one line why it's skipped and move on.
4. PLAN: draft a plan and post it via send_message (from="planner"), with your message starting exactly with "${PLAN_READY_PREFIX}" followed by the plan. Cite the specific files/paths you read and relied on.
5. DOUBT (optional): if the plan involves a non-trivial decision per the doubt-driven-development skill's own "When to Use" checklist, type "/doubt-driven-development" as your next message to run it. Otherwise say why it's skipped and move on.
6. RECONCILE (optional): if doubt or the interview surfaced real findings, loop back through architecture-interview once more, then re-post the revised plan via send_message prefixed exactly "${PLAN_READY_PREFIX}" again.

IMPORTANT — this environment is headless, there is no TTY, but AskUserQuestion IS real here:
- Whenever architecture-interview (or any skill) wants to ask "the user" a question, use the AskUserQuestion tool exactly as the skill instructs. It is wired to the web dashboard, not Discord: the call blocks (up to timeoutMs, default 60s) until answered there. If it returns {"status":"pending"}, call it again with the same questions to keep waiting.
- send_message is for everything else: narrative plan text and status updates.
- doubt-driven-development's cross-model escalation step is non-interactive here — skip it and announce the skip.

You are READ-ONLY and BASH-ONLY right now — write/edit will be denied until a human explicitly approves. Reply naturally to whatever the human says next; you don't need to call any tool to "wait" for them.`;
}

function implementPrompt(): string {
  return `Mohammad approved the plan. Implement it now in this worktree:
1. Write the code, following the plan you settled on.
2. Add or update tests; run the repo's test suite and make it pass. If there is no test suite, add one first.
3. If the task calls for e2e/browser/behavioral verification, call request_e2e_env to get a live pod running this branch plus Playwright browser-automation tools — use it, then call kill_env when you're done. Mention the preview URL in your reply.
4. Update docs if relevant.
5. Commit your changes (do not push — that happens after this session ends).
End your final message with a line exactly: PR_READY: <one-paragraph summary for the PR description>`;
}

async function logSdkMessage(actor: string, msg: { type: string; [key: string]: unknown }): Promise<void> {
  if (msg.type === "system" && msg.subtype === "init") {
    log("info", `${actor} session started`, {
      model: msg.model,
      mcpServers: msg.mcp_servers,
      permissionMode: msg.permissionMode,
      skills: msg.skills,
      plugins: msg.plugins,
    });
    return;
  }
  if (msg.type === "assistant") {
    const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
    for (const block of content) {
      if (block.type === "text" && typeof block.text === "string" && block.text.trim()) {
        log("info", `${actor} text`, { text: block.text });
        // Relayed through the transcript (core's own relay loop posts it to
        // Discord), not a direct Discord call — see sidecarClient.pushMessage.
        await sidecar
          .pushMessage(actor, block.text, "discussion")
          .catch((err) => log("warn", "pushMessage failed", { actor, error: String(err) }));
      }
      if (block.type === "tool_use") log("info", `${actor} tool_use`, { tool: block.name, input: block.input });
    }
    return;
  }
  if (msg.type === "user") {
    const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
    for (const block of content) {
      if (block.type === "tool_result") {
        log(block.is_error ? "error" : "info", `${actor} tool_result`, {
          isError: block.is_error ?? false,
          content: typeof block.content === "string" ? block.content.slice(0, 2000) : block.content,
        });
      }
    }
  }
}

export type TaskResult = { aborted: boolean; summary: string };

// runTask is the continuous-session driver (docs/adr/0021): one query()
// call, streaming-input mode, spans planning and implementation — no
// teardown/restart at the approval boundary. Replaces runPlanningPhase +
// runImplementationPhase.
export async function runTask(task: Task): Promise<TaskResult> {
  let approved = false;
  let aborted = false;
  let planCount = 0;
  let awaitingCheckpointReply = false;
  let finalText = "";

  const input = new InputQueue();
  input.push(plannerPrompt(task));

  const abortController = new AbortController();
  const q = query({
    prompt: input.stream(),
    options: {
      executable: "bun",
      cwd: WORKTREE_PATH,
      model: MODEL,
      permissionMode: "plan",
      // Write/Edit are deliberately never in this list — canUseTool below
      // is the live escalation path (confirmed in Phase 0's spike:
      // allowedTools bypasses canUseTool entirely for anything it lists,
      // so declaring them here would silently remove the gate, not add one).
      allowedTools: [
        "Read", "Glob", "Grep", "Bash", "WebSearch", "WebFetch", "Task",
        "mcp__agent-fleet-sidecar__send_message",
        "mcp__agent-fleet-sidecar__wait_for_messages",
        "mcp__agent-fleet-sidecar__AskUserQuestion",
        "mcp__agent-fleet-sidecar__request_e2e_env",
        "mcp__agent-fleet-sidecar__kill_env",
        "mcp__agent-fleet-sidecar__*",
      ],
      canUseTool: async (toolName, toolInput) => {
        if ((toolName === "Write" || toolName === "Edit") && !approved) {
          return { behavior: "deny", message: "Not approved yet — wait for explicit human approval before writing." };
        }
        return { behavior: "allow", updatedInput: toolInput };
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
      if (isAbort(entry)) {
        aborted = true;
        // interrupt() is graceful but only meaningful mid-turn — if the
        // session is idle (paused between rounds, waiting for the next
        // streamed input, the common case for an unprompted /stop),
        // there's nothing active for it to interrupt and the `for await`
        // loop below would otherwise hang forever waiting for a message
        // that will never come. abortController.abort() guarantees the
        // underlying session actually ends either way.
        await q.interrupt().catch(() => {});
        abortController.abort();
        return;
      }
      if (isApproval(entry)) {
        approved = true;
        awaitingCheckpointReply = false;
        await q.setPermissionMode("default");
        input.push(implementPrompt());
        // The planning->implementation boundary only exists inside this
        // function now (one continuous session, docs/adr/0021) — index.ts
        // has no other way to observe it, so this is the one place that
        // updates the status humans see on the dashboard/Discord.
        await sidecar
          .setStatus("implementing")
          .catch((err) => log("warn", "setStatus(implementing) failed", { taskId: task.id, error: String(err) }));
        return;
      }
      awaitingCheckpointReply = false;
      input.push(entry.text);
    }, humanMessagesAbort.signal)
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
      await logSdkMessage("planner", msg as { type: string; [key: string]: unknown });

      if (msg.type === "assistant") {
        const content = (msg.message as { content?: { type: string; [k: string]: unknown }[] })?.content ?? [];
        for (const block of content) {
          if (block.type === "text") finalText = block.text as string;
          if (block.type === "tool_use" && block.name === "mcp__agent-fleet-sidecar__send_message") {
            const text = (block.input as { text?: string })?.text ?? "";
            if (!approved && text.startsWith(PLAN_READY_PREFIX)) planCount++;
          }
        }
      }

      if (msg.type === "result") {
        if (aborted) break;

        if (approved) {
          if (msg.subtype === "success") break; // implementation finished
          const transient = msg.num_turns === 0 && msg.total_cost_usd === 0;
          const message = `implementation stopped: ${msg.subtype} after ${msg.num_turns} turns, $${msg.total_cost_usd}`;
          throw transient ? new TransientError(message) : new Error(message);
        }

        if (planCount >= MAX_PLANNING_ROUNDS && !awaitingCheckpointReply) {
          awaitingCheckpointReply = true;
          await q.interrupt();
          await sidecar
            .pushMessage(
              "planner",
              `Round ${planCount} done (${MAX_PLANNING_ROUNDS} plan draft${MAX_PLANNING_ROUNDS === 1 ? "" : "s"}) with no verdict yet. Reply to keep going, say "approved" to proceed, or "stop" to cancel.`,
              "discussion",
            )
            .catch((err) => log("warn", "pushMessage(checkpoint) failed", { taskId: task.id, error: String(err) }));
        }
        // Otherwise: not yet approved, not at the round cap — the session
        // naturally pauses here until the human-message consumer above
        // pushes the next input (a reply, or nothing yet).
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
