import { query } from "@anthropic-ai/claude-agent-sdk";
import { Redis } from "ioredis";
import type { Task } from "./db.js";

const MCP_REDIS_ENTRY = process.env.MCP_REDIS_ENTRY ?? "/app/mcp-redis/dist/index.js";
const MODEL = process.env.CLAUDE_MODEL ?? "claude-opus-4-8";
const PLANNING_TIMEOUT_MS = Number(process.env.PLANNING_TIMEOUT_MS ?? 0); // 0 = unbounded

const redisMcpServer = {
  type: "stdio" as const,
  command: "node",
  args: [MCP_REDIS_ENTRY],
  env: {
    REDIS_HOST: process.env.REDIS_HOST ?? "redis.bnei.lan",
    REDIS_PORT: process.env.REDIS_PORT ?? "6379",
    REDIS_MAIN_PASSWORD: process.env.REDIS_MAIN_PASSWORD ?? "",
  },
};

function isApproval(text: string): boolean {
  return /\bapprove(d)?\b|\blgtm\b|\bship it\b|\bgo ahead\b/i.test(text);
}

async function waitForHumanApproval(taskId: string): Promise<void> {
  const redis = new Redis({
    host: process.env.REDIS_HOST ?? "redis.bnei.lan",
    port: Number(process.env.REDIS_PORT ?? 6379),
    password: process.env.REDIS_MAIN_PASSWORD,
  });
  const key = `agentfleet:planning:${taskId}`;
  let cursor = 0;
  try {
    for (;;) {
      const raw = await redis.lrange(key, cursor, -1);
      for (const r of raw) {
        cursor++;
        const msg = JSON.parse(r) as { from: string; text: string };
        if (msg.from === "human" && isApproval(msg.text)) return;
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
  } finally {
    redis.disconnect();
  }
}

// Runs the proposer + critic as two independent Claude Code agentic sessions
// (Agent SDK's own tool-calling loop drives each side — we don't hand-roll
// turn-taking), coordinating only through the shared Redis transcript via the
// mcp-redis stdio server. Blocks until Mohammad approves in Discord (relayed
// into the same transcript by the bot). Returns the proposer's session id so
// implementation can resume the SAME session per mvp-spec's requirement.
export async function runPlanningPhase(task: Task): Promise<{ proposerSessionId: string }> {
  let proposerSessionId = "";

  const proposerPrompt = `You are the PROPOSER for task ${task.id} in repo ${task.repo}.
Task: ${task.description}

Read the actual repository (you are in a fresh git worktree on branch agent/${task.id}) and post an architecture/implementation plan using the send_message tool (taskId="${task.id}", from="proposer"). Then repeatedly call wait_for_messages (taskId="${task.id}") to read replies from the CRITIC and from Mohammad. Engage with the critic's objections and refine the plan via more send_message calls.

You are READ-ONLY and BASH-ONLY right now — do not write or edit any files.
Stop and end your turn only once a message from "human" contains explicit approval (e.g. "approved", "go ahead", "ship it"). Never infer approval from silence or from the critic alone.`;

  const criticPrompt = `You are the CRITIC for task ${task.id} in repo ${task.repo}.
Read the actual repository (read-only) and repeatedly call wait_for_messages (taskId="${task.id}") to see the PROPOSER's plan. Challenge it with real objections and alternatives grounded in the actual code — not a rubber-stamp pass. Post via send_message (taskId="${task.id}", from="critic").
Stop and end your turn once a message from "human" contains explicit approval.`;

  const proposerRun = (async () => {
    for await (const msg of query({
      prompt: proposerPrompt,
      options: {
        model: MODEL,
        cwd: `/workspace/worktrees/${task.id}`,
        permissionMode: "plan",
        allowedTools: ["Read", "Glob", "Grep", "Bash"],
        mcpServers: { "agent-fleet-redis": redisMcpServer },
      },
    })) {
      // Every SDKMessage variant carries session_id; grab it as soon as the
      // first one arrives so we can `resume` this exact session in phase 2.
      if (!proposerSessionId && "session_id" in msg) proposerSessionId = msg.session_id;
      if (msg.type === "result") break;
    }
  })();

  const criticRun = (async () => {
    for await (const msg of query({
      prompt: criticPrompt,
      options: {
        model: MODEL,
        cwd: `/workspace/worktrees/${task.id}`,
        permissionMode: "plan",
        allowedTools: ["Read", "Glob", "Grep", "Bash"],
        mcpServers: { "agent-fleet-redis": redisMcpServer },
      },
    })) {
      if (msg.type === "result") break;
    }
  })();

  const approvalWait = PLANNING_TIMEOUT_MS > 0
    ? Promise.race([
        waitForHumanApproval(task.id),
        new Promise<void>((_, reject) =>
          setTimeout(() => reject(new Error("planning timed out")), PLANNING_TIMEOUT_MS),
        ),
      ])
    : waitForHumanApproval(task.id);

  // Planning is unbounded/paced by Mohammad (per mvp-spec) — only the
  // post-approval implementation phase is timeout-bound.
  await approvalWait;
  await Promise.allSettled([proposerRun, criticRun]);

  return { proposerSessionId };
}

// Resumes the SAME proposer session (no restart) with write/edit/bash
// unlocked, and drives it through code -> tests -> docs -> PR.
export async function runImplementationPhase(
  task: Task,
  proposerSessionId: string,
): Promise<string> {
  const worktreePath = `/workspace/worktrees/${task.id}`;
  const implementPrompt = `Mohammad approved the plan. Implement it now in this worktree:
1. Write the code, following the plan you and the critic settled on.
2. Add or update tests; run the repo's test suite and make it pass. If there is no test suite, add one first.
3. Update docs if relevant.
4. Commit your changes.
End your final message with a line exactly: PR_READY: <one-paragraph summary for the PR description>`;

  let finalText = "";
  for await (const msg of query({
    prompt: implementPrompt,
    options: {
      resume: proposerSessionId,
      model: MODEL,
      cwd: worktreePath,
      permissionMode: "default",
      allowedTools: ["Read", "Glob", "Grep", "Bash", "Write", "Edit"],
      mcpServers: { "agent-fleet-redis": redisMcpServer },
    },
  })) {
    if (msg.type === "assistant") {
      const textBlock = msg.message?.content?.find((b: { type: string }) => b.type === "text");
      if (textBlock) finalText = textBlock.text;
    }
  }
  return finalText;
}
