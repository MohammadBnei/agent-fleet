// Exercises planning.ts's coordination logic — round-cap math, the
// skip_critique branch (ADR-0011), crash recovery, human approval, and the
// implementation-phase kill switch — without a real Claude session or
// fleet-core server. Mocks must be registered before importing planning.js
// (Bun resolves mock.module() calls ahead of subsequent static imports).
import { test, expect, mock, beforeEach } from "bun:test";
import type { Task } from "./db.js";

type TranscriptMsg = { from: string; text: string; type?: string };

// In-memory stand-in for fleet-core's planning_transcript, keyed by taskId
// (fleetCoreClient's readSince/currentCursor take taskId directly, unlike
// the old Redis key string) — both the mocked module and this file's own
// pushMsg() calls see the same list.
const transcriptStore = new Map<string, TranscriptMsg[]>();

function pushMsg(taskId: string, msg: TranscriptMsg): void {
  const list = transcriptStore.get(taskId) ?? [];
  list.push(msg);
  transcriptStore.set(taskId, list);
}

mock.module("./fleetCoreClient.js", () => ({
  readSince: async (taskId: string, sinceIndex: number) => {
    const list = transcriptStore.get(taskId) ?? [];
    const start = sinceIndex < 0 ? 0 : sinceIndex;
    return { messages: list.slice(start), nextIndex: list.length };
  },
  currentCursor: async (taskId: string) => (transcriptStore.get(taskId) ?? []).length,
}));

// appendJournal writes to real Postgres — stub it so tests never touch the
// network, regardless of what CI's DNS does with the unreachable prod host.
mock.module("./db.js", () => ({
  appendJournal: mock(async () => {}),
}));

// postReply hits the real Discord REST API — capture what would have been
// posted instead. Most tests never trigger it (discord_thread_id: null on
// makeTask's default), but the crash-recovery test below needs to inspect
// the checkpoint text to tell a session-ended checkpoint apart from a
// round-cap one.
const discordPosts: string[] = [];
mock.module("./discord.js", () => ({
  postReply: mock(async (_threadId: string, content: string) => {
    discordPosts.push(content);
  }),
}));

// Records every fake query() invocation so tests can assert call count and
// role — the actual signal that skip_critique/round-cap logic worked.
let queryCalls: Array<{ role: "proposer" | "critic" }> = [];

// Lets one test force the critic's fake session to crash immediately after
// starting (simulating error_max_turns / a real crash), to verify watchBatch
// reacts with a "session ended" checkpoint instead of hanging forever.
let crashCriticForTaskId: string | null = null;

// Lets a test slow the fake session down past a poll interval, so a
// concurrently-arriving abort message has time to win the race instead of
// the (near-instant) fake generator finishing first.
let queryDelayMs = 0;

function taskIdFromCwd(cwd: string | undefined): string {
  const m = cwd?.match(/\/workspace\/worktrees\/(.+)$/);
  if (!m) throw new Error(`fake query(): could not extract taskId from cwd "${cwd}"`);
  return m[1];
}

// Every real proposer/critic prompt in planning.ts includes its own
// send_message call with from="proposer"/from="critic" — reuse that exact
// substring to tell the two roles apart instead of guessing at prompt shape.
// (The implementation-phase prompt contains neither, so it falls back to
// "proposer" — accurate, since it really does resume the proposer session.)
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
    if (queryDelayMs > 0) await Bun.sleep(queryDelayMs);
    if (role === "critic" && taskId === crashCriticForTaskId) {
      throw new Error("simulated critic crash");
    }
    pushMsg(taskId, { from: role, text: `mock ${role} message` });
    yield {
      type: "assistant",
      message: { content: [{ type: "text", text: `mock ${role} message` }] },
    };
    yield { type: "result", subtype: "success", num_turns: 1, total_cost_usd: 0 };
  },
}));

const { runPlanningPhase, runImplementationPhase } = await import("./planning.js");

beforeEach(() => {
  queryCalls = [];
  crashCriticForTaskId = null;
  queryDelayMs = 0;
  discordPosts.length = 0;
});

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: crypto.randomUUID(),
    repo: "dream-analyst",
    description: "test task",
    status: "planning",
    discord_channel_id: "chan",
    discord_thread_id: null, // keeps postReply() unreachable by default — every call site is gated on this
    claimed_by: "test-worker",
    pr_url: null,
    skip_critique: false,
    ...overrides,
  };
}

test(
  "skip_critique=true never spawns a critic session",
  async () => {
    const task = makeTask({ skip_critique: true });

    const phase = runPlanningPhase(task);
    // Give the round-cap checkpoint a chance to fire, then abort at the
    // checkpoint so the phase resolves instead of waiting on a human forever.
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });

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
    const task = makeTask({ skip_critique: false });

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
    expect(queryCalls.filter((c) => c.role === "proposer").length).toBe(1);
    expect(queryCalls.filter((c) => c.role === "critic").length).toBe(1);
  },
  10000,
);

test(
  "human approval resolves the phase and returns the proposer's session id",
  async () => {
    const task = makeTask({ skip_critique: true });

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "approved", type: "approve" });

    const result = await phase;

    expect(result.aborted).toBe(false);
    expect(result.proposerSessionId).toMatch(/^proposer-/);
  },
  10000,
);

test(
  "a crashed critic session triggers a session-ended checkpoint, not a silent hang",
  async () => {
    const task = makeTask({ skip_critique: false, discord_thread_id: "thread-1" });
    crashCriticForTaskId = task.id;

    const phase = runPlanningPhase(task);
    // watchBatch polls every ~1s; wait a full cycle so it has definitely
    // already detected flags.criticEnded and posted the session-ended
    // checkpoint before this test's own abort message can race it and get
    // picked up as a plain kill-switch instead.
    await Bun.sleep(1300);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
    expect(discordPosts.some((m) => m.includes("crashed"))).toBe(true);
  },
  10000,
);

test(
  "a stop mid-implementation aborts instead of reporting success",
  async () => {
    queryDelayMs = 1500; // outlast waitForCheckpointReply's ~1s poll so the abort wins the race
    const task = makeTask({ skip_critique: true });

    const phase = runImplementationPhase(task, "proposer-fixed-session");
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
  },
  10000,
);

test(
  "implementation completes and returns the session's final text",
  async () => {
    const task = makeTask({ skip_critique: true });

    const result = await runImplementationPhase(task, "proposer-fixed-session");

    expect(result.aborted).toBe(false);
    expect(result.summary).toContain("mock proposer message");

    // runImplementationPhase's stopWatcher has no timeout and only exits on
    // an abort message — having lost the Promise.race here, it's still
    // polling fleet-core in the background. Harmless in production (the worker
    // process moves on to push+PR and keeps running regardless), but it
    // would leak a live 1s timer past this test in a short-lived test
    // process. Give it the abort it's waiting for before moving on.
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });
    await Bun.sleep(1200);
  },
  10000,
);
