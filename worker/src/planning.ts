import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Task } from "./db.js";
import { appendJournal } from "./db.js";
import { postReply } from "./discord.js";
import { log } from "./log.js";
import { currentCursor, readSince } from "./fleetCoreClient.js";

const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const PLANNING_TIMEOUT_MS = Number(process.env.PLANNING_TIMEOUT_MS ?? 0); // 0 = unbounded, per mvp-spec

// Guardrails. Defaults are deliberately tight — cheaper to make Mohammad ask
// for another round than to let two agents debate unsupervised for an hour.
const MAX_PLANNING_ROUNDS = Number(process.env.MAX_PLANNING_ROUNDS ?? 1);
// No default cap — fixed defaults (15, then 40, then 100) all turned out too
// tight for genuine exploration of an unfamiliar codebase (confirmed live:
// the proposer burned 15 turns tracing a real Prisma panic across 4+ files
// and never even reached send_message). maxTurns is opt-in now: unset envs
// mean unbounded, set one only if a specific run needs capping.
const MAX_TURNS_PLANNING = process.env.MAX_TURNS_PLANNING ? Number(process.env.MAX_TURNS_PLANNING) : undefined;
const MAX_TURNS_IMPLEMENTATION = process.env.MAX_TURNS_IMPLEMENTATION
  ? Number(process.env.MAX_TURNS_IMPLEMENTATION)
  : undefined;
// No maxBudgetUsd either: Claude Code auths via CLAUDE_CODE_OAUTH_TOKEN
// (subscription), not metered API billing — total_cost_usd is a notional
// figure the SDK still computes for reporting, not a real charge.

// allowedTools only *auto-approves* — tools missing from it still exist but
// get their permission request silently denied in a headless query() (no
// canUseTool handler, no TTY to prompt). Without these, the critic/proposer
// can never actually call send_message/wait_for_messages: confirmed live —
// burned all 15 turns on denied calls in under a minute, $1.1+, zero
// transcript entries either side.
const FLEET_CORE_MCP_TOOLS = [
  "mcp__agent-fleet-core__send_message",
  "mcp__agent-fleet-core__wait_for_messages",
];

const FLEET_CORE_URL = process.env.FLEET_CORE_URL ?? "http://fleet-core.agent-fleet.svc.cluster.local:8080";

function fleetCoreMcpServer() {
  return { type: "http" as const, url: `${FLEET_CORE_URL}/mcp` };
}

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

type TranscriptEntry = { from: string; text: string; type?: "discussion" | "approve" | "abort" };

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

// Set by runPlanningPhase's finally blocks when a proposer/critic query()
// loop exits — for any reason (normal completion, error_max_turns, crash).
// Lets watchBatch stop waiting on a round that can now never complete,
// instead of polling forever (PLANNING_TIMEOUT_MS defaults to 0/unbounded).
type SessionFlags = { proposerEnded: boolean; criticEnded: boolean };

// Polls the shared transcript for the duration of one proposer/critic batch.
// Returns as soon as any of: explicit human approval, explicit human
// stop/abort, the round cap is hit (maxRounds exchanges with no verdict from
// Mohammad), either session ends without reaching that cap (crash/turn
// limit/early return — the round can't complete on its own anymore), or the
// overall wall-clock budget (if set) runs out. Also relays every
// proposer/critic message to Discord as it lands, so the debate is visible
// live instead of only at round checkpoints.
async function watchBatch(
  task: Task,
  sinceIndex: number,
  maxRounds: number,
  timeoutMs: number,
  flags: SessionFlags,
  criticEnabled: boolean,
): Promise<WatchOutcome> {
  const deadline = timeoutMs > 0 ? Date.now() + timeoutMs : Infinity;
  let cursor = sinceIndex;
  let proposerCount = 0;
  let criticCount = 0;
  while (Date.now() < deadline) {
    const { messages, nextIndex } = await readSince(task.id, cursor);
    cursor = nextIndex;
    for (const msg of messages as TranscriptEntry[]) {
      if (msg.from === "human" && isAbort(msg)) {
        log("info", "human abort received during planning batch", { taskId: task.id, text: msg.text });
        return { type: "aborted" };
      }
      if (msg.from === "human" && isApproval(msg)) return { type: "approved" };
      if (msg.from === "proposer" || msg.from === "critic") {
        if (msg.from === "proposer") proposerCount++;
        else criticCount++;
        if (task.discord_thread_id) {
          await postReply(task.discord_thread_id, `**${msg.from}:** ${msg.text}`);
        }
      }
    }
    const roundsSoFar = criticEnabled ? Math.min(proposerCount, criticCount) : proposerCount;
    if (roundsSoFar >= maxRounds) {
      return { type: "round_cap", nextIndex: cursor };
    }
    if (flags.proposerEnded || (criticEnabled && flags.criticEnded)) {
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

function proposerPrompt(task: Task, resuming: boolean): string {
  if (!resuming) {
    return `You are the PROPOSER for task ${task.id} in repo ${task.repo}.
Task: ${task.description}

Read the actual repository (you are in a fresh git worktree on branch agent/${task.id}) and post an architecture/implementation plan using the send_message tool (taskId="${task.id}", from="proposer"). Cite the specific files/paths you read and relied on so the critic can start from your findings instead of re-reading the repo cold. Then call wait_for_messages (taskId="${task.id}") to read replies from the CRITIC and from Mohammad, and respond to what you find. Do not loop indefinitely — you'll be re-invoked for the next round automatically, so it's fine to end your turn after one exchange.

You are READ-ONLY and BASH-ONLY right now — do not write or edit any files.`;
  }
  return `Continue the planning discussion for task ${task.id}. Call wait_for_messages (taskId="${task.id}") to see what Mohammad and the critic said since you last checked, respond via send_message (taskId="${task.id}", from="proposer"), then end your turn. Still read-only.`;
}

function criticPrompt(task: Task, resuming: boolean): string {
  if (!resuming) {
    return `You are the CRITIC for task ${task.id} in repo ${task.repo}.
Call wait_for_messages (taskId="${task.id}") to read the PROPOSER's plan — it should cite the files/paths it relied on. Start from those instead of re-reading the repo cold; only Read/Glob/Grep further to verify a specific claim or cover something the proposer didn't address. Challenge the plan with real objections and alternatives grounded in the actual code — not a rubber-stamp pass. Post via send_message (taskId="${task.id}", from="critic"), then end your turn.`;
  }
  return `Continue reviewing task ${task.id}. Call wait_for_messages (taskId="${task.id}") to see what's new, respond via send_message (taskId="${task.id}", from="critic") — either push back further or say plainly that you're satisfied — then end your turn.`;
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
// separate it from the proposer/critic's deliberate transcript messages.
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

// Runs the proposer + critic as independent Claude Code agentic sessions
// (Agent SDK's own tool-calling loop drives each side), coordinating only
// through the shared Redis transcript. Every MAX_PLANNING_ROUNDS exchanges
// without a verdict from Mohammad, both sessions are aborted and a
// checkpoint is posted to Discord — planning never runs away unsupervised
// for more than one round-batch at a time. A "stop"/"abort" reply from
// Mohammad kills it immediately at any point, not just at a checkpoint.
export async function runPlanningPhase(
  task: Task,
): Promise<{ proposerSessionId: string; aborted: boolean }> {
  const criticEnabled = !task.skip_critique;
  let proposerSessionId = "";
  let criticSessionId = "";
  let cursor = 0;
  let batch = 0;

  for (;;) {
    batch++;
    const resuming = batch > 1;
    const proposerAbort = new AbortController();
    const criticAbort = new AbortController();
    const flags: SessionFlags = { proposerEnded: false, criticEnded: false };

    const proposerRun = (async () => {
      try {
        for await (const msg of query({
          prompt: proposerPrompt(task, resuming),
          options: {
            executable: "bun",
            ...(resuming ? { resume: proposerSessionId } : {}),
            model: MODEL,
            cwd: `/workspace/worktrees/${task.id}`,
            permissionMode: "plan",
            allowedTools: ["Read", "Glob", "Grep", "Bash", "WebSearch", "WebFetch", ...FLEET_CORE_MCP_TOOLS],
            mcpServers: { "agent-fleet-core": fleetCoreMcpServer() },
            maxTurns: MAX_TURNS_PLANNING,
            abortController: proposerAbort,
          },
        })) {
          if (!proposerSessionId && "session_id" in msg) proposerSessionId = msg.session_id;
          await logSdkMessage("proposer", msg, task.discord_thread_id);
          if (msg.type === "result") await logResult("proposer", task.repo, msg);
        }
      } catch {
        // Expected when we abort on round-cap/kill-switch — nothing to do.
      } finally {
        flags.proposerEnded = true;
      }
    })();

    // Skipped entirely (not just fast-forwarded) when task.skip_critique is
    // set — Mohammad's explicit opt-out on /task, never inferred from task
    // size by the proposer itself (see ADR-0011).
    const criticRun = !criticEnabled
      ? Promise.resolve()
      : (async () => {
          try {
            for await (const msg of query({
              prompt: criticPrompt(task, resuming),
              options: {
                executable: "bun",
                ...(resuming ? { resume: criticSessionId } : {}),
                model: MODEL,
                cwd: `/workspace/worktrees/${task.id}`,
                permissionMode: "plan",
                allowedTools: ["Read", "Glob", "Grep", "Bash", "WebSearch", "WebFetch", ...FLEET_CORE_MCP_TOOLS],
                mcpServers: { "agent-fleet-core": fleetCoreMcpServer() },
                maxTurns: MAX_TURNS_PLANNING,
                abortController: criticAbort,
              },
            })) {
              if (!criticSessionId && "session_id" in msg) criticSessionId = msg.session_id;
              await logSdkMessage("critic", msg, task.discord_thread_id);
              if (msg.type === "result") await logResult("critic", task.repo, msg);
            }
          } catch {
            // Expected when we abort.
          } finally {
            flags.criticEnded = true;
          }
        })();

    const outcome = await watchBatch(task, cursor, MAX_PLANNING_ROUNDS, PLANNING_TIMEOUT_MS, flags, criticEnabled);
    proposerAbort.abort();
    criticAbort.abort();
    await Promise.allSettled([proposerRun, criticRun]);

    if (outcome.type === "approved") return { proposerSessionId, aborted: false };
    if (outcome.type === "aborted") return { proposerSessionId, aborted: true };
    if (outcome.type === "timeout") throw new Error("planning timed out waiting for a verdict");

    // round_cap or session_ended: checkpoint with Mohammad before spending anything further.
    cursor = outcome.nextIndex;
    if (task.discord_thread_id) {
      const checkpointMsg =
        outcome.type === "session_ended"
          ? `A planning session ended without reaching a decision (crashed, hit its turn/budget limit, or returned early). Reply to retry another round, say "approved" to proceed with whatever plan exists so far, or "stop" to cancel.`
          : `Round ${batch} done (${MAX_PLANNING_ROUNDS} ${criticEnabled ? "proposer<->critic exchange" : "proposer turn"}${MAX_PLANNING_ROUNDS === 1 ? "" : "s"}) with no verdict yet. Reply to keep going, say "approved" to proceed with the current plan, or "stop" to cancel this task.`;
      await postReply(task.discord_thread_id, checkpointMsg);
    }
    const reply = await waitForCheckpointReply(task.id, cursor, PLANNING_TIMEOUT_MS);
    if (reply === "aborted") return { proposerSessionId, aborted: true };
    if (reply === "timeout") throw new Error("planning timed out waiting for a checkpoint reply");
    // "continue" — loop back into another round-batch, resuming both sessions.
  }
}

// Resumes the SAME proposer session (no restart) with write/edit/bash
// unlocked, and drives it through code -> tests -> docs -> PR. Still
// killable mid-flight: a "stop"/"abort" reply in the thread aborts this too,
// not just the planning phase.
export async function runImplementationPhase(
  task: Task,
  proposerSessionId: string,
): Promise<{ summary: string; aborted: boolean }> {
  const worktreePath = `/workspace/worktrees/${task.id}`;
  const implementPrompt = `Mohammad approved the plan. Implement it now in this worktree:
1. Write the code, following the plan you and the critic settled on.
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
        resume: proposerSessionId,
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
      await logSdkMessage("proposer", msg, task.discord_thread_id);
      if (msg.type === "assistant") {
        const textBlock = msg.message?.content?.find((b: { type: string }) => b.type === "text");
        if (textBlock) finalText = textBlock.text;
      }
      if (msg.type === "result") {
        await logResult("proposer", task.repo, msg);
        if (msg.subtype !== "success") {
          throw new Error(
            `implementation stopped: ${msg.subtype} after ${msg.num_turns} turns, $${msg.total_cost_usd}`,
          );
        }
      }
    }
  })();

  const aborted = await Promise.race([implementationRun.then(() => false), stopWatcher]);
  abortController.abort();

  return { summary: finalText, aborted: Boolean(aborted) };
}
