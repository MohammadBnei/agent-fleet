import { query, type SDKUserMessage, type PermissionResult } from "@anthropic-ai/claude-agent-sdk";
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

// Absolute in-container path baked at image build time — the local skills
// plugin (doubt-driven-development, architecture-interview), bundled the
// same way as before this rewrite (see docs/adr/0017). Must be absolute:
// the session's cwd is the target repo's worktree, not this one.
const PLANNING_SKILLS_PLUGIN_PATH = "/app/worker/skills/agent-fleet-planning";

function sidecarMcpServer() {
  return { type: "http" as const, url: `http://${SIDECAR_MCP_ADDR}/mcp` };
}

// Bash used to sit in allowedTools, which meant canUseTool was never even
// consulted for it (allowedTools bypasses canUseTool entirely for
// anything it lists — same reason Write/Edit are deliberately kept out of
// it, see below). A worker confused by a Write denial reached the same
// effect via `Bash cp`/`mkdir` before real approval — confirmed live in
// prod. This is a verb/redirection denylist, not a real sandbox.
// ponytail: heuristic, not a sandbox — a determined bypass (base64 | some
// decoder, etc.) can still slip through. Good enough to close the
// demonstrated cp/mkdir bypass and the obvious write vectors. Upgrade
// path if that's ever exploited for real: run pre-approval Bash against a
// read-only bind mount of the worktree instead of out-guessing shell
// syntax.
const MUTATING_BASH_RE =
  /(^|[;&|]\s*)(cp|mv|rm|rmdir|mkdir|touch|tee|dd|chmod|chown|ln|truncate|npm\s+install|pnpm\s+add|yarn\s+add)\b|\bsed\s+(-\w*i|--in-place)|\bgit\s+(add|commit|checkout|reset|apply|stash|rm|mv|clean)\b|>(?!=|&)/;

function isMutatingBashCommand(command: string): boolean {
  return MUTATING_BASH_RE.test(command);
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

// One continuous session, planning and implementation both — no fleet-
// imposed phase boundary, no PLAN_READY:/checkpoint-round machinery
// (reliability-findings.md #0 supersedes #3/#4's regex/magic-string bugs
// by deleting the mechanism, not patching it). The agent decides for
// itself when it's explored/planned enough and when to start
// implementing; canUseTool below is the only real gate — ExitPlanMode
// blocks on a genuine human decision, and Write/Edit/mutating-Bash are
// denied until approved.
function taskPrompt(task: Task): string {
  return `You are working on task ${task.id} in repo ${task.repo}.
Task: ${task.description}

You are in a git worktree on branch agent/${task.id} — it may be freshly created or resumed from a prior attempt (the provisioner reuses an existing worktree as-is rather than wiping it, reliability-findings.md #2), so run \`git status\` and \`git log --oneline -5\` first to see what's actually there before assuming a clean slate.

This is one continuous session, not separate planning/implementation phases — decide for yourself when you've explored and planned enough to implement. Use your own judgment about how much process a task this size actually needs; skip a stage and say why in one line rather than run it on autopilot:

1. EXPLORE: read the actual repository — don't guess at structure or conventions.
2. REVIEW: check your own findings for gaps before drafting anything.
3. INTERVIEW (optional): if this involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill's own DOOR classification), type "/architecture-interview" as your next message to run it. Otherwise say in one line why it's skipped and move on.
4. PLAN: post your plan via send_message (from="planner"). Cite the specific files/paths you read and relied on, then call ExitPlanMode with that same plan.
5. DOUBT (optional): if the plan involves a non-trivial decision per the doubt-driven-development skill's own "When to Use" checklist, type "/doubt-driven-development" as your next message to run it. Otherwise say why it's skipped and move on.
6. RECONCILE (optional): if doubt or the interview surfaced real findings, loop back through architecture-interview once more, then re-post the revised plan.

You are READ-ONLY and read-only-BASH-ONLY until Mohammad explicitly approves. ExitPlanMode IS the approval prompt — like Claude Code's own plan mode, that call blocks until Mohammad actually responds (Approve, or a specific permission mode, from the dashboard); its tool result tells you what was decided, so just call it and read the answer, no separate wait/poll loop needed. A denied ExitPlanMode with a message means Mohammad replied with feedback, not approval — incorporate it and call ExitPlanMode again when ready. Write/Edit, and any Bash command that would modify files, are denied with an explanation until real approval lands — that's not a bug, it just means approval hasn't happened yet. Never state or imply in your own messages that an approval, an answer, or a permission-mode change has happened — a successful Write/Edit call, an ExitPlanMode call that returns allow, or a real AskUserQuestion tool result are the only evidence that's real; if you're not sure, it hasn't happened yet.

Once approved, implement the plan in this same session, same worktree:
1. Write the code, following the plan you settled on.
2. Add or update tests; run the repo's test suite and make it pass. If there is no test suite, add one first.
3. If the task calls for e2e/browser/behavioral verification, call request_e2e_env to get a live pod running this branch plus Playwright browser-automation tools — use it, then call kill_env when you're done. Mention the preview URL in your reply.
4. Update docs if relevant.
5. Commit your changes with git yourself via Bash, then push and open the PR yourself: \`git push -u origin agent/${task.id}\`, then \`gh pr create --title "..." --body "..." --base ${task.baseBranch}\`. Your git identity is already configured — just run the commands. Confirm the PR actually exists afterward (e.g. \`gh pr view\`) before declaring done.
6. End your final message with a line exactly: PR_READY: <one-paragraph summary for the PR description>

IMPORTANT — this environment is headless, there is no TTY, but AskUserQuestion IS real here:
- Whenever architecture-interview (or any skill) wants to ask "the user" a question, use the AskUserQuestion tool exactly as the skill instructs. It is wired to the web dashboard, not Discord: the call blocks (up to timeoutMs, default 60s) until answered there. If it returns {"status":"pending"}, call it again with the same questions to keep waiting.
- send_message is for everything else: narrative plan text and status updates.
- doubt-driven-development's cross-model escalation step is non-interactive here — skip it and announce the skip.`;
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
  let approved = false;
  let aborted = false;
  let finalText = "";

  // Set only while a real ExitPlanMode call is in flight — canUseTool's
  // ExitPlanMode branch below awaits this instead of trusting the SDK's
  // own canned "plan approved" text, so the tool call itself blocks until
  // a genuine human decision arrives. This makes plan-exit work the same
  // way it does in Claude Code CLI: the exit-plan-mode prompt IS the
  // approval gate, not a side channel disconnected from it.
  let pendingPlanDecision: { resolve: (result: PermissionResult) => void; input: Record<string, unknown> } | null = null;

  // build receives the original ExitPlanMode tool input so an "allow"
  // result can pass it through unchanged (PermissionResult.allow requires
  // updatedInput). No-op if no ExitPlanMode call is actually pending.
  function resolvePendingPlan(build: (input: Record<string, unknown>) => PermissionResult): void {
    const pending = pendingPlanDecision;
    pendingPlanDecision = null;
    if (pending) pending.resolve(build(pending.input));
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
      permissionMode: "plan",
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
      canUseTool: async (toolName, toolInput) => {
        // ExitPlanMode blocks here until a real human decision resolves it
        // (see streamHumanMessages below) — the SDK's own built-in
        // ExitPlanMode success text is not evidence of real approval in
        // this architecture, so we never just let it through.
        if (toolName === "ExitPlanMode") {
          return new Promise<PermissionResult>((resolve) => {
            pendingPlanDecision = { resolve, input: toolInput as Record<string, unknown> };
          });
        }
        if (!approved) {
          if (toolName === "Write" || toolName === "Edit") {
            return { behavior: "deny", message: "Not approved yet — wait for explicit human approval before writing." };
          }
          if (toolName === "Bash" && isMutatingBashCommand((toolInput as { command?: string }).command ?? "")) {
            return {
              behavior: "deny",
              message:
                "Not approved yet — this Bash command would modify files. Read-only exploration (git status/log/diff, ls, find, cat, grep, ...) is fine; file mutation waits for explicit human approval.",
            };
          }
        }
        return { behavior: "allow", updatedInput: toolInput as Record<string, unknown> };
      },
      mcpServers: { "agent-fleet-sidecar": sidecarMcpServer() },
      plugins: [{ type: "local", path: PLANNING_SKILLS_PLUGIN_PATH }],
      maxTurns: MAX_TURNS,
      abortController,
    },
  });

  // Delivers every new human message live (docs/adr/0021 point 2) — not a
  // poll the wrapper initiates on its own schedule. Only structured
  // type:"approve"/"abort" signals resolve the phase now
  // (reliability-findings.md #0/#3: the old free-text word-matching
  // fallback false-positived on phrases like "I don't approve this").
  const humanMessagesAbort = new AbortController();
  const humanMessagesDone = sidecar
    .streamHumanMessages(async (entry) => {
      if (entry.from !== "human") return;
      if (entry.type === "answer") return; // consumed by the blocked AskUserQuestion tool call server-side
      if (entry.type === "abort") {
        aborted = true;
        // A pending ExitPlanMode call has nothing left to wait for — deny
        // it so the promise doesn't dangle past the session teardown below.
        resolvePendingPlan(() => ({ behavior: "deny", message: "Mohammad stopped the task.", interrupt: true }));
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
      if (entry.type === "approve" && !approved) {
        approved = true;
        await q.setPermissionMode("default");
        // If ExitPlanMode is the one waiting on this, resolving it IS the
        // signal the model sees — no separate input.push needed (or
        // wanted: it'd be a second, redundant "you're approved" message).
        if (pendingPlanDecision) {
          resolvePendingPlan((planInput) => ({ behavior: "allow", updatedInput: planInput }));
        } else {
          input.push("Mohammad approved the plan — implement it now, following the instructions already given.");
        }
        // The planning->implementation boundary only exists inside this
        // function now (one continuous session, docs/adr/0021) — index.ts
        // has no other way to observe it, so this is the one place that
        // updates the status humans see on the dashboard/Discord.
        await sidecar
          .setStatus("implementing")
          .catch((err) => log("warn", "setStatus(implementing) failed", { taskId: task.id, error: String(err) }));
        return;
      }
      // The dashboard's permission-mode selector (docs/adr/0027) — additive
      // to the binary approve above, reachable only from the dashboard, not
      // Discord. `approved=true` only matters for modes canUseTool still
      // gets consulted for; bypassPermissions skips canUseTool entirely at
      // the SDK level regardless (verified against the bundled SDK source,
      // see ADR-0027).
      if (entry.type === "permission_mode") {
        const mode = entry.text as "acceptEdits" | "dontAsk" | "bypassPermissions";
        approved = true;
        await q.setPermissionMode(mode);
        if (pendingPlanDecision) {
          resolvePendingPlan((planInput) => ({ behavior: "allow", updatedInput: planInput }));
        } else {
          input.push(`Mohammad set the permission mode to ${mode} — continue accordingly.`);
        }
        await sidecar
          .setStatus("implementing")
          .catch((err) => log("warn", "setStatus(implementing) failed", { taskId: task.id, error: String(err) }));
        return;
      }
      if (pendingPlanDecision) {
        // A plain reply while ExitPlanMode is waiting is feedback, not
        // approval — deny with the human's own text as the reason (same as
        // choosing "No, keep planning" with guidance in Claude Code CLI's
        // own prompt: interrupt stays unset so the model incorporates the
        // reply and keeps going). It can call ExitPlanMode again once
        // it's revised the plan.
        resolvePendingPlan(() => ({ behavior: "deny", message: `Mohammad replied (not an approval): ${entry.text}` }));
        return;
      }
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
        }
      }

      if (msg.type === "result") {
        if (aborted) break;
        // Uniform non-success handling (reliability-findings.md #8) — no
        // more approved/!approved branch split, since there's no longer a
        // fleet-imposed phase boundary to split on. A non-success result
        // at any point in the session is treated the same way.
        if (msg.subtype === "success") {
          if (approved) break; // implementation finished
          // Not yet approved, session paused naturally (idle, waiting for
          // the next streamed input — a reply, or nothing yet). Keep
          // consuming; the human-message consumer above pushes the next
          // input whenever a reply arrives.
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
