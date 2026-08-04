import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Task } from "./db.js";
import { appendJournal, saveSessionIds } from "./db.js";

// Thrown when an implementation-phase SDK result looks like a transient
// infra/SDK hiccup (0 turns, $0 cost) rather than a genuine implementation
// failure — the caller requeues instead of failing the task outright. See
// docs/adr/0016.
export class TransientError extends Error {}
import { postReply } from "./discord.js";
import { log } from "./log.js";
import { currentCursor, readSince } from "./fleetCoreClient.js";

const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const PLANNING_TIMEOUT_MS = Number(process.env.PLANNING_TIMEOUT_MS ?? 0); // 0 = unbounded, per mvp-spec

// Guardrails. Defaults are deliberately tight — cheaper to make Mohammad ask
// for another round than to let the planner run unsupervised for an hour.
const MAX_PLANNING_ROUNDS = Number(process.env.MAX_PLANNING_ROUNDS ?? 1);
// No default cap — fixed defaults (15, then 40, then 100) all turned out too
// tight for genuine exploration of an unfamiliar codebase (confirmed live: a
// planning session burned 15 turns tracing a real Prisma panic across 4+
// files and never even reached send_message). maxTurns is opt-in now: unset
// envs mean unbounded, set one only if a specific run needs capping.
const MAX_TURNS_PLANNING = process.env.MAX_TURNS_PLANNING ? Number(process.env.MAX_TURNS_PLANNING) : undefined;
const MAX_TURNS_IMPLEMENTATION = process.env.MAX_TURNS_IMPLEMENTATION
  ? Number(process.env.MAX_TURNS_IMPLEMENTATION)
  : undefined;
// No maxBudgetUsd either: Claude Code auths via CLAUDE_CODE_OAUTH_TOKEN
// (subscription), not metered API billing — total_cost_usd is a notional
// figure the SDK still computes for reporting, not a real charge.

// allowedTools only *auto-approves* — tools missing from it still exist but
// get their permission request silently denied in a headless query() (no
// canUseTool handler, no TTY to prompt). Without these, the planner can
// never actually call send_message/wait_for_messages: confirmed live —
// burned all 15 turns on denied calls in under a minute, $1.1+, zero
// transcript entries either side.
const FLEET_CORE_MCP_TOOLS = [
  "mcp__agent-fleet-core__send_message",
  "mcp__agent-fleet-core__wait_for_messages",
  "mcp__agent-fleet-core__AskUserQuestion",
];

const FLEET_CORE_URL = process.env.FLEET_CORE_URL ?? "http://fleet-core.agent-fleet.svc.cluster.local:8080";

function fleetCoreMcpServer() {
  return { type: "http" as const, url: `${FLEET_CORE_URL}/mcp` };
}

// Absolute in-container path baked at image build time (worker/Dockerfile's
// `COPY worker ./worker` copies this whole tree) — not env-configurable like
// FLEET_CORE_URL/E2E_PROVISIONER_URL, since there's no deployment variance to
// configure around, only one image. Bundles doubt-driven-development and
// architecture-interview as a local plugin the planner invokes itself — see
// worker/skills/agent-fleet-planning/README.md and docs/adr/0017. Must be an
// absolute path: the planning query()'s cwd is the *target repo's* worktree,
// not this one, so project-relative skill discovery (settingSources:
// ['project']) would look in the wrong place entirely.
const PLANNING_SKILLS_PLUGIN_PATH = "/app/worker/skills/agent-fleet-planning";

// Prefix the planner uses on a send_message post specifically when it's a
// complete, reviewable plan (first draft or a post-doubt/interview
// revision) — round-cap counting keys off this, not off every message, so
// an in-flight interview (each question is its own send_message/
// wait_for_messages round-trip) doesn't trip a checkpoint mid-conversation.
const PLAN_READY_PREFIX = "PLAN_READY:";

// Implementation-phase only (see runImplementationPhase) — e2e testing only
// makes sense once code changes exist, not during planning. request_e2e_env
// and kill_env are always present; Playwright's own tool names (e.g.
// mcp__agent-fleet-e2e__browser_navigate) aren't listed individually here —
// they only exist once request_e2e_env has actually created a pod, so
// enumerating them upfront isn't possible. Allowing the whole
// "mcp__agent-fleet-e2e__*" prefix is required for those to work at all;
// same silent-permission-denial trap as FLEET_CORE_MCP_TOOLS above (ADR-0008) —
// verify this prefix form is actually honored by the SDK during real e2e
// testing, don't assume it from this comment alone.
const E2E_MCP_TOOLS = [
  "mcp__agent-fleet-e2e__request_e2e_env",
  "mcp__agent-fleet-e2e__kill_env",
  "mcp__agent-fleet-e2e__*",
];

const E2E_PROVISIONER_URL =
  process.env.E2E_PROVISIONER_URL ?? "http://e2e-provisioner.agent-fleet.svc.cluster.local:8080";

function e2eMcpServer(task: Task) {
  return {
    type: "http" as const,
    url: `${E2E_PROVISIONER_URL}/mcp/${task.id}`,
  };
}

type TranscriptEntry = {
  from: string;
  text: string;
  type?: "discussion" | "approve" | "abort" | "question" | "answer";
};

// /approve and /stop are unambiguous — checked first. The word-matching
// fallback is only for anyone who types "approved"/"stop" as plain text
// instead of using the slash command.
function isApproval(msg: TranscriptEntry): boolean {
  if (msg.type === "approve") return true;
  if (msg.type === "abort") return false;
  return /\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b/i.test(msg.text);
}

// The manual kill switch: works at any point in planning OR implementation,
// not just at a round-cap checkpoint. Mohammad types it (or uses /stop) in
// the thread; the bot relays it into the transcript exactly like any other
// reply.
function isAbort(msg: TranscriptEntry): boolean {
  if (msg.type === "abort") return true;
  if (msg.type === "approve") return false;
  return /\b(stop|abort|cancel|kill)\b/i.test(msg.text);
}

type WatchOutcome =
  | { type: "approved" }
  | { type: "aborted" }
  | { type: "round_cap"; nextIndex: number }
  | { type: "session_ended"; nextIndex: number }
  | { type: "timeout" };

// Set by runPlanningPhase's finally block when the planner's query() loop
// exits — for any reason (normal completion, error_max_turns, crash). Lets
// watchBatch stop waiting on a round that can now never complete, instead of
// polling forever (PLANNING_TIMEOUT_MS defaults to 0/unbounded).
type SessionFlags = { sessionEnded: boolean };

// Polls the shared transcript for the duration of one planning batch.
// Returns as soon as any of: explicit human approval, explicit human
// stop/abort, the round cap is hit (maxRounds PLAN_READY: posts with no
// verdict from Mohammad — NOT maxRounds messages; interview questions and
// doubt-cycle status relay live but don't count), the session ends without
// reaching that cap (crash/turn limit/early return), or the overall
// wall-clock budget (if set) runs out. Relays every planner message to
// Discord as it lands, so the process is visible live instead of only at
// round checkpoints.
async function watchBatch(
  task: Task,
  sinceIndex: number,
  maxRounds: number,
  timeoutMs: number,
  flags: SessionFlags,
): Promise<WatchOutcome> {
  const deadline = timeoutMs > 0 ? Date.now() + timeoutMs : Infinity;
  let cursor = sinceIndex;
  let planCount = 0;
  while (Date.now() < deadline) {
    const { messages, nextIndex } = await readSince(task.id, cursor);
    cursor = nextIndex;
    for (const msg of messages as TranscriptEntry[]) {
      // Dashboard-submitted structured answers are consumed directly by the
      // blocked AskUserQuestion MCP tool call server-side (docs/adr/0018) —
      // never interpreted here. Without this, a human's chosen option label
      // containing a word like "approved" or "stop" could false-positive
      // isApproval/isAbort's word-matching fallback below.
      if (msg.type === "answer") continue;
      if (msg.from === "human" && isAbort(msg)) {
        log("info", "human abort received during planning batch", { taskId: task.id, text: msg.text });
        return { type: "aborted" };
      }
      if (msg.from === "human" && isApproval(msg)) return { type: "approved" };
      if (msg.from === "planner") {
        if (msg.type === "question") {
          if (task.discord_thread_id) {
            await postReply(task.discord_thread_id, "**planner:** asked a question — answer it on the dashboard.");
          }
          continue; // raw JSON payload, not a plan — never counts toward the round cap
        }
        if (msg.text.startsWith(PLAN_READY_PREFIX)) planCount++;
        if (task.discord_thread_id) {
          await postReply(task.discord_thread_id, `**planner:** ${msg.text}`);
        }
      }
    }
    if (planCount >= maxRounds) {
      return { type: "round_cap", nextIndex: cursor };
    }
    if (flags.sessionEnded) {
      return { type: "session_ended", nextIndex: cursor };
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  return { type: "timeout" };
}

// Blocks until Mohammad sends *any* reply (or an abort) after a round-cap
// checkpoint — unbounded by design (per mvp-spec, planning is paced by
// Mohammad, not a clock), unless PLANNING_TIMEOUT_MS is explicitly set.
async function waitForCheckpointReply(
  taskId: string,
  sinceIndex: number,
  timeoutMs: number,
): Promise<"continue" | "aborted" | "timeout"> {
  const deadline = timeoutMs > 0 ? Date.now() + timeoutMs : Infinity;
  let cursor = sinceIndex;
  while (Date.now() < deadline) {
    const { messages, nextIndex } = await readSince(taskId, cursor);
    cursor = nextIndex;
    for (const msg of messages as TranscriptEntry[]) {
      if (msg.from === "human") {
        const abort = isAbort(msg);
        if (abort) log("info", "human abort received", { taskId, text: msg.text });
        return abort ? "aborted" : "continue";
      }
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  return "timeout";
}

// The pipeline is deliberately described, not hardcoded — the planner
// decides for itself, per task complexity, whether interview/doubt stages
// apply (no /task-time override knob; see docs/adr/0017). Both skills are
// bundled as a local plugin (PLANNING_SKILLS_PLUGIN_PATH) and invoked by
// typing "/architecture-interview" / "/doubt-driven-development" as a
// message, same as a human would in an interactive session. Their
// interactive question-asking resolves through a real AskUserQuestion MCP
// tool answered via the web dashboard, not Discord — Discord's plain-text
// threads can't render structured multiple-choice UI (see docs/adr/0018).
function plannerPrompt(task: Task, resuming: boolean): string {
  if (!resuming) {
    return `You are the PLANNER for task ${task.id} in repo ${task.repo}.
Task: ${task.description}

You are in a fresh git worktree on branch agent/${task.id}. Work through this pipeline, using your own judgment about how much of it a task this size actually needs — skip a stage and say why in one line rather than run it on autopilot:

1. EXPLORE: read the actual repository — don't guess at structure or conventions.
2. REVIEW: check your own findings for gaps before drafting anything.
3. INTERVIEW (optional): if this involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill's own DOOR classification), type "/architecture-interview" as your next message to run it. Otherwise say in one line why it's skipped (e.g. "two-way door, low blast radius") and move on.
4. PLAN: draft a plan and post it via send_message (taskId="${task.id}", from="planner"), with your message starting exactly with "${PLAN_READY_PREFIX}" followed by the plan. Cite the specific files/paths you read and relied on.
5. DOUBT (optional): if the plan involves a non-trivial decision per the doubt-driven-development skill's own "When to Use" checklist (branching logic, crosses a boundary, asserts an unverifiable property, irreversible blast radius), type "/doubt-driven-development" as your next message to run it. Otherwise say why it's skipped and move on.
6. RECONCILE (optional): if doubt or the interview surfaced real findings or open questions, loop back through architecture-interview once more, then re-post the revised plan via send_message prefixed exactly "${PLAN_READY_PREFIX}" again.
7. Call wait_for_messages (taskId="${task.id}") to read replies from Mohammad, and respond to what you find. Do not loop indefinitely — you'll be re-invoked for the next round automatically, so it's fine to end your turn after one exchange.

IMPORTANT — this environment is headless, there is no TTY, but AskUserQuestion IS real here:
- Whenever architecture-interview (or any skill) wants to ask "the user" a question, use the AskUserQuestion tool exactly as the skill instructs — one question at a time, your hypothesis attached, options with a label and description. It is wired to the web dashboard, not Discord: the call blocks (up to timeoutMs, default 60s) until Mohammad answers there. If it returns {"status":"pending"}, no one has answered yet — call it again with the same questions to keep waiting; do not treat "pending" as "skip this question" or fall back to guessing.
- send_message/wait_for_messages are for everything else: narrative plan text, status updates ("skipping doubt: ..."), and reading Mohammad's replies/approval — not for asking him a question with real options, that's what AskUserQuestion is for now.
- doubt-driven-development's cross-model escalation step (offering to shell out to a gemini/codex CLI) is genuinely non-interactive here — skip it and announce the skip in your message, per that skill's own non-interactive-context rule. Never invoke an external CLI.

You are READ-ONLY and BASH-ONLY right now — do not write or edit any files.`;
  }
  return `Continue the planning discussion for task ${task.id}. Call wait_for_messages (taskId="${task.id}") to see what Mohammad said since you last checked, and continue the pipeline above from wherever you left off (interview/doubt/plan revision as needed) — respond via send_message (taskId="${task.id}", from="planner"), prefixing any complete revised plan with "${PLAN_READY_PREFIX}", then end your turn. Still read-only.`;
}

async function logResult(
  actor: string,
  repo: string,
  msg: { subtype?: string; num_turns?: number; total_cost_usd?: number; permission_denials?: unknown[] },
): Promise<void> {
  await appendJournal(repo, actor, "session.result", {
    subtype: msg.subtype,
    numTurns: msg.num_turns,
    totalCostUsd: msg.total_cost_usd,
    permissionDenials: msg.permission_denials?.length ?? 0,
  });
}

// Streams every SDK message to stdout as it arrives (not just the final
// result) — `kubectl logs -f` otherwise goes silent for minutes at a time
// while a session is actually working, and previously gave zero visibility
// into permission denials (the actual cause of the mcp__agent-fleet-redis
// tools-not-allowed bug — would have shown up here immediately as
// "tool_result" entries with isError: true instead of being diagnosed after
// the fact from cost/turn-count/timing). Also relays the assistant's own
// text (its reasoning, not just its formal send_message posts) to Discord
// as it's generated — this is the raw thinking-out-loud, quoted to visually
// separate it from the planner's deliberate transcript messages. Logs
// `skills`/`plugins` on session start too — the fastest live signal that
// PLANNING_SKILLS_PLUGIN_PATH actually loaded (see docs/adr/0017's open
// verification items).
async function logSdkMessage(
  actor: string,
  msg: { type: string; [key: string]: unknown },
  discordThreadId: string | null,
): Promise<void> {
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
      if (block.type === "text") {
        log("info", `${actor} text`, { text: block.text });
        if (discordThreadId && typeof block.text === "string" && block.text.trim()) {
          await postReply(discordThreadId, `> **${actor}:** ${block.text}`);
        }
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

// Runs the planner as a single Claude Code agentic session (Agent SDK's own
// tool-calling loop), following the explore/review/interview/plan/doubt
// pipeline described in plannerPrompt — the planner decides for itself
// whether interview/doubt stages apply, and can invoke doubt-driven-
// development's fresh-context Task-tool subagent for adversarial review
// instead of a second independent SDK session (see docs/adr/0017,
// superseding docs/adr/0002's proposer+critic design). Every
// MAX_PLANNING_ROUNDS PLAN_READY: posts without a verdict from Mohammad, the
// session is aborted and a checkpoint is posted to Discord — planning never
// runs away unsupervised for more than one round-batch at a time. A
// "stop"/"abort" reply from Mohammad kills it immediately at any point, not
// just at a checkpoint.
export async function runPlanningPhase(
  task: Task,
): Promise<{ planningSessionId: string; aborted: boolean }> {
  let planningSessionId = "";
  let cursor = 0;
  let batch = 0;

  for (;;) {
    batch++;
    const resuming = batch > 1;
    const plannerAbort = new AbortController();
    const flags: SessionFlags = { sessionEnded: false };

    const plannerRun = (async () => {
      try {
        for await (const msg of query({
          prompt: plannerPrompt(task, resuming),
          options: {
            executable: "bun",
            ...(resuming ? { resume: planningSessionId } : {}),
            model: MODEL,
            cwd: `/workspace/worktrees/${task.id}`,
            permissionMode: "plan",
            allowedTools: ["Read", "Glob", "Grep", "Bash", "WebSearch", "WebFetch", "Task", ...FLEET_CORE_MCP_TOOLS],
            plugins: [{ type: "local", path: PLANNING_SKILLS_PLUGIN_PATH }],
            mcpServers: { "agent-fleet-core": fleetCoreMcpServer() },
            maxTurns: MAX_TURNS_PLANNING,
            abortController: plannerAbort,
          },
        })) {
          if (!planningSessionId && "session_id" in msg) {
            planningSessionId = msg.session_id;
            await saveSessionIds(task.id, planningSessionId);
          }
          await logSdkMessage("planner", msg, task.discord_thread_id);
          if (msg.type === "result") await logResult("planner", task.repo, msg);
        }
      } catch {
        // Expected when we abort on round-cap/kill-switch — nothing to do.
      } finally {
        flags.sessionEnded = true;
      }
    })();

    const outcome = await watchBatch(task, cursor, MAX_PLANNING_ROUNDS, PLANNING_TIMEOUT_MS, flags);
    plannerAbort.abort();
    await plannerRun;

    if (outcome.type === "approved") return { planningSessionId, aborted: false };
    if (outcome.type === "aborted") return { planningSessionId, aborted: true };
    if (outcome.type === "timeout") throw new Error("planning timed out waiting for a verdict");

    // round_cap or session_ended: checkpoint with Mohammad before spending anything further.
    cursor = outcome.nextIndex;
    if (task.discord_thread_id) {
      const checkpointMsg =
        outcome.type === "session_ended"
          ? `The planning session ended without reaching a decision (crashed, hit its turn/budget limit, or returned early). Reply to retry another round, say "approved" to proceed with whatever plan exists so far, or "stop" to cancel.`
          : `Round ${batch} done (${MAX_PLANNING_ROUNDS} plan draft${MAX_PLANNING_ROUNDS === 1 ? "" : "s"}) with no verdict yet. Reply to keep going, say "approved" to proceed with the current plan, or "stop" to cancel this task.`;
      await postReply(task.discord_thread_id, checkpointMsg);
    }
    const reply = await waitForCheckpointReply(task.id, cursor, PLANNING_TIMEOUT_MS);
    if (reply === "aborted") return { planningSessionId, aborted: true };
    if (reply === "timeout") throw new Error("planning timed out waiting for a checkpoint reply");
    // "continue" — loop back into another round-batch, resuming the session.
  }
}

// Resumes the SAME planner session (no restart) with write/edit/bash
// unlocked, and drives it through code -> tests -> docs -> PR. Still
// killable mid-flight: a "stop"/"abort" reply in the thread aborts this too,
// not just the planning phase.
export async function runImplementationPhase(
  task: Task,
  planningSessionId: string,
): Promise<{ summary: string; aborted: boolean }> {
  const worktreePath = `/workspace/worktrees/${task.id}`;
  const implementPrompt = `Mohammad approved the plan. Implement it now in this worktree:
1. Write the code, following the plan you settled on.
2. Add or update tests; run the repo's test suite and make it pass. If there is no test suite, add one first.
3. If the task calls for e2e/browser/behavioral verification, call request_e2e_env to get a live pod running this branch plus Playwright browser-automation tools — use it, then call kill_env when you're done with it (it also gets torn down automatically once this task finishes). Mention the preview URL it returns in your reply so Mohammad can check it himself.
4. Update docs if relevant.
5. Commit your changes.
End your final message with a line exactly: PR_READY: <one-paragraph summary for the PR description>`;

  const abortController = new AbortController();
  let finalText = "";
  let cursor = 0;
  try {
    cursor = await currentCursor(task.id);
  } catch {
    // best-effort — if this fails the abort watcher just starts from 0
  }

  const stopWatcher = (async (): Promise<boolean> => {
    for (;;) {
      const r = await waitForCheckpointReply(task.id, cursor, 0);
      if (r === "aborted") {
        abortController.abort();
        return true;
      }
      if (r === "timeout") return false;
      cursor += 1; // keep advancing past non-abort replies rather than re-triggering on the same message
    }
  })();

  const implementationRun = (async () => {
    for await (const msg of query({
      prompt: implementPrompt,
      options: {
        executable: "bun",
        resume: planningSessionId,
        model: MODEL,
        cwd: worktreePath,
        permissionMode: "default",
        allowedTools: [
          "Read", "Glob", "Grep", "Bash", "Write", "Edit", "WebSearch", "WebFetch",
          ...FLEET_CORE_MCP_TOOLS,
          ...E2E_MCP_TOOLS,
        ],
        mcpServers: { "agent-fleet-core": fleetCoreMcpServer(), "agent-fleet-e2e": e2eMcpServer(task) },
        maxTurns: MAX_TURNS_IMPLEMENTATION,
        abortController,
      },
    })) {
      await logSdkMessage("planner", msg, task.discord_thread_id);
      if (msg.type === "assistant") {
        const textBlock = msg.message?.content?.find((b: { type: string }) => b.type === "text");
        if (textBlock) finalText = textBlock.text;
      }
      if (msg.type === "result") {
        await logResult("planner", task.repo, msg);
        if (msg.subtype !== "success") {
          const message = `implementation stopped: ${msg.subtype} after ${msg.num_turns} turns, $${msg.total_cost_usd}`;
          // 0 turns / $0 means the SDK never actually got started — almost
          // certainly a transient infra hiccup, not a real implementation
          // failure (this is exactly the shape of the incident that
          // motivated docs/adr/0016).
          const transient = msg.num_turns === 0 && msg.total_cost_usd === 0;
          throw transient ? new TransientError(message) : new Error(message);
        }
      }
    }
  })();

  const aborted = await Promise.race([implementationRun.then(() => false), stopWatcher]);
  abortController.abort();

  return { summary: finalText, aborted: Boolean(aborted) };
}
