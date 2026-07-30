import { query } from "@anthropic-ai/claude-agent-sdk";
import { Redis } from "ioredis";
import type { Task } from "./db.js";
import { appendJournal } from "./db.js";
import { postReply } from "./discord.js";

const MCP_REDIS_ENTRY = process.env.MCP_REDIS_ENTRY ?? "/app/mcp-redis/src/index.ts";
const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const PLANNING_TIMEOUT_MS = Number(process.env.PLANNING_TIMEOUT_MS ?? 0); // 0 = unbounded, per mvp-spec

// Guardrails. Defaults are deliberately tight — cheaper to make Mohammad ask
// for another round than to let two agents debate unsupervised for an hour.
const MAX_PLANNING_ROUNDS = Number(process.env.MAX_PLANNING_ROUNDS ?? 1);
const MAX_TURNS_PLANNING = Number(process.env.MAX_TURNS_PLANNING ?? 15);
const MAX_TURNS_IMPLEMENTATION = Number(process.env.MAX_TURNS_IMPLEMENTATION ?? 40);
const MAX_BUDGET_USD_PLANNING = Number(process.env.MAX_BUDGET_USD_PLANNING ?? 2);
const MAX_BUDGET_USD_IMPLEMENTATION = Number(process.env.MAX_BUDGET_USD_IMPLEMENTATION ?? 5);

function redisClient(): Redis {
  return new Redis({
    host: process.env.REDIS_HOST ?? "redis.bnei.lan",
    port: Number(process.env.REDIS_PORT ?? 6379),
    password: process.env.REDIS_MAIN_PASSWORD,
  });
}

function planningKey(taskId: string): string {
  return `agentfleet:planning:${taskId}`;
}

function mcpServer() {
  return {
    type: "stdio" as const,
    command: "bun",
    args: ["run", MCP_REDIS_ENTRY],
    env: {
      REDIS_HOST: process.env.REDIS_HOST ?? "redis.bnei.lan",
      REDIS_PORT: process.env.REDIS_PORT ?? "6379",
      REDIS_MAIN_PASSWORD: process.env.REDIS_MAIN_PASSWORD ?? "",
    },
  };
}

function isApproval(text: string): boolean {
  return /\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b/i.test(text);
}

// The manual kill switch: works at any point in planning OR implementation,
// not just at a round-cap checkpoint. Mohammad types it in the thread; the
// bot relays it into the transcript exactly like any other reply.
function isAbort(text: string): boolean {
  return /\b(stop|abort|cancel|kill)\b/i.test(text);
}

type WatchOutcome =
  | { type: "approved" }
  | { type: "aborted" }
  | { type: "round_cap"; nextIndex: number }
  | { type: "timeout" };

// Polls the shared transcript for the duration of one proposer/critic batch.
// Returns as soon as any of: explicit human approval, explicit human
// stop/abort, the round cap is hit (maxRounds exchanges with no verdict from
// Mohammad), or the overall wall-clock budget (if set) runs out.
async function watchBatch(
  taskId: string,
  sinceIndex: number,
  maxRounds: number,
  timeoutMs: number,
): Promise<WatchOutcome> {
  const redis = redisClient();
  const key = planningKey(taskId);
  const deadline = timeoutMs > 0 ? Date.now() + timeoutMs : Infinity;
  let cursor = sinceIndex;
  let proposerCount = 0;
  let criticCount = 0;
  try {
    while (Date.now() < deadline) {
      const raw = await redis.lrange(key, cursor, -1);
      for (const r of raw) {
        cursor++;
        const msg = JSON.parse(r) as { from: string; text: string };
        if (msg.from === "human" && isAbort(msg.text)) return { type: "aborted" };
        if (msg.from === "human" && isApproval(msg.text)) return { type: "approved" };
        if (msg.from === "proposer") proposerCount++;
        if (msg.from === "critic") criticCount++;
      }
      if (Math.min(proposerCount, criticCount) >= maxRounds) {
        return { type: "round_cap", nextIndex: cursor };
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
    return { type: "timeout" };
  } finally {
    redis.disconnect();
  }
}

// Blocks until Mohammad sends *any* reply (or an abort) after a round-cap
// checkpoint — unbounded by design (per mvp-spec, planning is paced by
// Mohammad, not a clock), unless PLANNING_TIMEOUT_MS is explicitly set.
async function waitForCheckpointReply(
  taskId: string,
  sinceIndex: number,
  timeoutMs: number,
): Promise<"continue" | "aborted" | "timeout"> {
  const redis = redisClient();
  const key = planningKey(taskId);
  const deadline = timeoutMs > 0 ? Date.now() + timeoutMs : Infinity;
  let cursor = sinceIndex;
  try {
    while (Date.now() < deadline) {
      const raw = await redis.lrange(key, cursor, -1);
      for (const r of raw) {
        cursor++;
        const msg = JSON.parse(r) as { from: string; text: string };
        if (msg.from === "human") return isAbort(msg.text) ? "aborted" : "continue";
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
    return "timeout";
  } finally {
    redis.disconnect();
  }
}

function proposerPrompt(task: Task, resuming: boolean): string {
  if (!resuming) {
    return `You are the PROPOSER for task ${task.id} in repo ${task.repo}.
Task: ${task.description}

Read the actual repository (you are in a fresh git worktree on branch agent/${task.id}) and post an architecture/implementation plan using the send_message tool (taskId="${task.id}", from="proposer"). Then call wait_for_messages (taskId="${task.id}") to read replies from the CRITIC and from Mohammad, and respond to what you find. Do not loop indefinitely — you'll be re-invoked for the next round automatically, so it's fine to end your turn after one exchange.

You are READ-ONLY and BASH-ONLY right now — do not write or edit any files.`;
  }
  return `Continue the planning discussion for task ${task.id}. Call wait_for_messages (taskId="${task.id}") to see what Mohammad and the critic said since you last checked, respond via send_message (taskId="${task.id}", from="proposer"), then end your turn. Still read-only.`;
}

function criticPrompt(task: Task, resuming: boolean): string {
  if (!resuming) {
    return `You are the CRITIC for task ${task.id} in repo ${task.repo}.
Read the actual repository (read-only) and call wait_for_messages (taskId="${task.id}") to see the PROPOSER's plan. Challenge it with real objections and alternatives grounded in the actual code — not a rubber-stamp pass. Post via send_message (taskId="${task.id}", from="critic"), then end your turn.`;
  }
  return `Continue reviewing task ${task.id}. Call wait_for_messages (taskId="${task.id}") to see what's new, respond via send_message (taskId="${task.id}", from="critic") — either push back further or say plainly that you're satisfied — then end your turn.`;
}

async function logResult(
  actor: string,
  repo: string,
  msg: { subtype?: string; num_turns?: number; total_cost_usd?: number },
): Promise<void> {
  await appendJournal(repo, actor, "session.result", {
    subtype: msg.subtype,
    numTurns: msg.num_turns,
    totalCostUsd: msg.total_cost_usd,
  });
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
  let proposerSessionId = "";
  let criticSessionId = "";
  let cursor = 0;
  let batch = 0;

  for (;;) {
    batch++;
    const resuming = batch > 1;
    const proposerAbort = new AbortController();
    const criticAbort = new AbortController();

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
            allowedTools: ["Read", "Glob", "Grep", "Bash"],
            mcpServers: { "agent-fleet-redis": mcpServer() },
            maxTurns: MAX_TURNS_PLANNING,
            maxBudgetUsd: MAX_BUDGET_USD_PLANNING,
            abortController: proposerAbort,
          },
        })) {
          if (!proposerSessionId && "session_id" in msg) proposerSessionId = msg.session_id;
          if (msg.type === "result") await logResult("proposer", task.repo, msg);
        }
      } catch {
        // Expected when we abort on round-cap/kill-switch — nothing to do.
      }
    })();

    const criticRun = (async () => {
      try {
        for await (const msg of query({
          prompt: criticPrompt(task, resuming),
          options: {
            executable: "bun",
            ...(resuming ? { resume: criticSessionId } : {}),
            model: MODEL,
            cwd: `/workspace/worktrees/${task.id}`,
            permissionMode: "plan",
            allowedTools: ["Read", "Glob", "Grep", "Bash"],
            mcpServers: { "agent-fleet-redis": mcpServer() },
            maxTurns: MAX_TURNS_PLANNING,
            maxBudgetUsd: MAX_BUDGET_USD_PLANNING,
            abortController: criticAbort,
          },
        })) {
          if (!criticSessionId && "session_id" in msg) criticSessionId = msg.session_id;
          if (msg.type === "result") await logResult("critic", task.repo, msg);
        }
      } catch {
        // Expected when we abort.
      }
    })();

    const outcome = await watchBatch(task.id, cursor, MAX_PLANNING_ROUNDS, PLANNING_TIMEOUT_MS);
    proposerAbort.abort();
    criticAbort.abort();
    await Promise.allSettled([proposerRun, criticRun]);

    if (outcome.type === "approved") return { proposerSessionId, aborted: false };
    if (outcome.type === "aborted") return { proposerSessionId, aborted: true };
    if (outcome.type === "timeout") throw new Error("planning timed out waiting for a verdict");

    // round_cap: checkpoint with Mohammad before spending anything further.
    cursor = outcome.nextIndex;
    if (task.discord_thread_id) {
      await postReply(
        task.discord_thread_id,
        `Round ${batch} done (${MAX_PLANNING_ROUNDS} proposer<->critic exchange${MAX_PLANNING_ROUNDS === 1 ? "" : "s"}) with no verdict yet. Reply to keep going, say "approved" to proceed with the current plan, or "stop" to cancel this task.`,
      );
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
3. Update docs if relevant.
4. Commit your changes.
End your final message with a line exactly: PR_READY: <one-paragraph summary for the PR description>`;

  const abortController = new AbortController();
  let finalText = "";
  let cursor = 0;
  try {
    const redis = redisClient();
    cursor = await redis.llen(planningKey(task.id));
    redis.disconnect();
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
        allowedTools: ["Read", "Glob", "Grep", "Bash", "Write", "Edit"],
        mcpServers: { "agent-fleet-redis": mcpServer() },
        maxTurns: MAX_TURNS_IMPLEMENTATION,
        maxBudgetUsd: MAX_BUDGET_USD_IMPLEMENTATION,
        abortController,
      },
    })) {
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
