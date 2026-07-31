// Exercises runPlanningPhase's coordination logic — round-cap math and the
// skip_critique branch added in ADR-0011 — without a real Claude session or
// Redis server. Mocks must be registered before importing planning.js (Bun
// resolves mock.module() calls ahead of subsequent static imports).
import { test, expect, mock } from "bun:test";
import type { Task } from "./db.js";

type TranscriptMsg = { from: string; text: string; type?: string };

// In-memory stand-in for the shared Redis planning transcript. Keyed exactly
// like planningKey() in planning.ts so both the mocked ioredis client and
// this file's own pushMsg() calls see the same list.
const redisStore = new Map<string, TranscriptMsg[]>();

function pushMsg(key: string, msg: TranscriptMsg): void {
  const list = redisStore.get(key) ?? [];
  list.push(msg);
  redisStore.set(key, list);
}

mock.module("ioredis", () => {
  class FakeRedis {
    async lrange(key: string, start: number): Promise<string[]> {
      const list = redisStore.get(key) ?? [];
      return list.slice(start < 0 ? 0 : start).map((m) => JSON.stringify(m));
    }
    async llen(key: string): Promise<number> {
      return (redisStore.get(key) ?? []).length;
    }
    disconnect(): void {}
  }
  return { Redis: FakeRedis, default: FakeRedis };
});

// appendJournal writes to real Postgres — stub it so tests never touch the
// network, regardless of what CI's DNS does with the unreachable prod host.
mock.module("./db.js", () => ({
  appendJournal: mock(async () => {}),
}));

// Records every fake query() invocation so tests can assert call count and
// role — the actual signal that skip_critique/round-cap logic worked.
let queryCalls: Array<{ role: "proposer" | "critic" }> = [];

function taskIdFromCwd(cwd: string | undefined): string {
  const m = cwd?.match(/\/workspace\/worktrees\/(.+)$/);
  if (!m) throw new Error(`fake query(): could not extract taskId from cwd "${cwd}"`);
  return m[1];
}

// Every real proposer/critic prompt in planning.ts includes its own
// send_message call with from="proposer"/from="critic" — reuse that exact
// substring to tell the two roles apart instead of guessing at prompt shape.
mock.module("@anthropic-ai/claude-agent-sdk", () => ({
  query: async function* ({
    prompt,
    options,
  }: {
    prompt: string;
    options?: { cwd?: string };
  }) {
    const role: "proposer" | "critic" = prompt.includes('from="critic"') ? "critic" : "proposer";
    queryCalls.push({ role });
    const taskId = taskIdFromCwd(options?.cwd);
    yield { type: "system", subtype: "init", session_id: `${role}-${crypto.randomUUID()}` };
    pushMsg(`agentfleet:planning:${taskId}`, { from: role, text: `mock ${role} message` });
    yield {
      type: "assistant",
      message: { content: [{ type: "text", text: `mock ${role} message` }] },
    };
    yield { type: "result", subtype: "success", num_turns: 1, total_cost_usd: 0 };
  },
}));

const { runPlanningPhase } = await import("./planning.js");

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: crypto.randomUUID(),
    repo: "dream-analyst",
    description: "test task",
    status: "planning",
    discord_channel_id: "chan",
    discord_thread_id: null, // keeps postReply() (real Discord fetch) unreachable — every call site is gated on this
    claimed_by: "test-worker",
    pr_url: null,
    skip_critique: false,
    ...overrides,
  };
}

test(
  "skip_critique=true never spawns a critic session",
  async () => {
    queryCalls = [];
    const task = makeTask({ skip_critique: true });
    const key = `agentfleet:planning:${task.id}`;

    const phase = runPlanningPhase(task);
    // Give the round-cap checkpoint a chance to fire, then abort at the
    // checkpoint so the phase resolves instead of waiting on a human forever.
    await Bun.sleep(50);
    pushMsg(key, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
    expect(queryCalls.length).toBe(1);
    expect(queryCalls[0].role).toBe("proposer");
  },
  10000,
);

test(
  "skip_critique=false (default) requires both a proposer and a critic session",
  async () => {
    queryCalls = [];
    const task = makeTask({ skip_critique: false });
    const key = `agentfleet:planning:${task.id}`;

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(key, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
    expect(queryCalls.filter((c) => c.role === "proposer").length).toBe(1);
    expect(queryCalls.filter((c) => c.role === "critic").length).toBe(1);
  },
  10000,
);
