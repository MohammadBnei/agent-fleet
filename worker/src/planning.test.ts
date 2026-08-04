// Exercises planning.ts's coordination logic — the PLAN_READY: round-cap
// convention, crash recovery, human approval, tool/plugin wiring, and the
// implementation-phase kill switch — without a real Claude session or
// fleet-core server. Mocks must be registered before importing planning.js
// (Bun resolves mock.module() calls ahead of subsequent static imports).
import { test, expect, mock, beforeEach } from "bun:test";
import type { Task } from "./db.js";

type TranscriptMsg = { from: string; text: string; type?: string };

const PLAN_READY_PREFIX = "PLAN_READY:";

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

// appendJournal/saveSessionIds write to real Postgres — stub them so tests
// never touch the network, regardless of what CI's DNS does with the
// unreachable prod host.
mock.module("./db.js", () => ({
  appendJournal: mock(async () => {}),
  saveSessionIds: mock(async () => {}),
}));

// postReply hits the real Discord REST API — capture what would have been
// posted instead. Most tests never trigger it (discord_thread_id: null on
// makeTask's default), but the crash-recovery and round-cap tests below need
// to inspect the checkpoint text to tell a session-ended checkpoint apart
// from a round-cap one.
const discordPosts: string[] = [];
mock.module("./discord.js", () => ({
  postReply: mock(async (_threadId: string, content: string) => {
    discordPosts.push(content);
  }),
}));

// Records every fake query() invocation's options — the actual signal that
// tool/plugin wiring (Task tool, the local skills plugin, no write/edit in
// plan mode) is correct, since there's only one session role now.
let queryCalls: Array<{ options: Record<string, unknown> }> = [];

// Lets one test force the fake planner session to crash immediately after
// starting (simulating error_max_turns / a real crash), to verify watchBatch
// reacts with a "session ended" checkpoint instead of hanging forever.
let crashPlannerForTaskId: string | null = null;

// Lets a test slow the fake session down past a poll interval, so a
// concurrently-arriving abort message has time to win the race instead of
// the (near-instant) fake generator finishing first.
let queryDelayMs = 0;

// Controls the text of the transcript message the fake session posts —
// defaults to a PLAN_READY: post (the common case: a session that produces
// a plan). Round-cap-specific tests override this to a non-prefixed message
// to prove interview/doubt-cycle chatter doesn't trip the checkpoint.
let mockMessageText = `${PLAN_READY_PREFIX} mock planner message`;

// Lets a test force the SDK's final `result` message for one task, to
// exercise runImplementationPhase's transient-vs-terminal classification
// (docs/adr/0016) without a real crash.
let forceResultForTaskId: string | null = null;
let forceResult: { subtype: string; num_turns: number; total_cost_usd: number } | null = null;

function taskIdFromCwd(cwd: string | undefined): string {
  const m = cwd?.match(/\/workspace\/worktrees\/(.+)$/);
  if (!m) throw new Error(`fake query(): could not extract taskId from cwd "${cwd}"`);
  return m[1];
}

mock.module("@anthropic-ai/claude-agent-sdk", () => ({
  query: async function* ({
    prompt: _prompt,
    options,
  }: {
    prompt: string;
    options: Record<string, unknown> & { cwd?: string };
  }) {
    queryCalls.push({ options });
    const taskId = taskIdFromCwd(options?.cwd);
    yield { type: "system", subtype: "init", session_id: `planner-${crypto.randomUUID()}` };
    if (queryDelayMs > 0) await Bun.sleep(queryDelayMs);
    if (taskId === crashPlannerForTaskId) {
      throw new Error("simulated planner crash");
    }
    pushMsg(taskId, { from: "planner", text: mockMessageText });
    yield {
      type: "assistant",
      message: { content: [{ type: "text", text: "mock planner message" }] },
    };
    if (taskId === forceResultForTaskId && forceResult) {
      yield { type: "result", ...forceResult };
      return;
    }
    yield { type: "result", subtype: "success", num_turns: 1, total_cost_usd: 0 };
  },
}));

const { runPlanningPhase, runImplementationPhase, TransientError } = await import("./planning.js");

beforeEach(() => {
  queryCalls = [];
  crashPlannerForTaskId = null;
  queryDelayMs = 0;
  discordPosts.length = 0;
  mockMessageText = `${PLAN_READY_PREFIX} mock planner message`;
  forceResultForTaskId = null;
  forceResult = null;
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
    planning_session_id: null,
    retry_count: 0,
    last_error: null,
    heartbeat_at: null,
    lease_id: null,
    ...overrides,
  };
}

test(
  "the planning session runs with plan-mode tool wiring: Task + the local skills plugin, no write/edit",
  async () => {
    const task = makeTask();

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });
    await phase;

    expect(queryCalls.length).toBe(1);
    const { options } = queryCalls[0];
    expect(options.permissionMode).toBe("plan");
    expect(options.allowedTools).toContain("Task");
    expect(options.allowedTools).toContain("mcp__agent-fleet-core__AskUserQuestion");
    expect(options.allowedTools).not.toContain("Write");
    expect(options.allowedTools).not.toContain("Edit");
    const plugins = options.plugins as Array<{ type: string; path: string }>;
    expect(plugins.some((p) => p.type === "local" && p.path.includes("agent-fleet-planning"))).toBe(true);
  },
  10000,
);

test(
  "a dashboard-submitted answer entry is never misread as approval/abort",
  async () => {
    const task = makeTask();

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    // A human's chosen option label could itself contain a word
    // isApproval's word-matching fallback matches (docs/adr/0018) — an
    // "answer"-type entry must never resolve the phase on its own.
    pushMsg(task.id, { from: "human", type: "answer", text: '{"answers":{"q":"approved, ship it"}}' });
    await Bun.sleep(200);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });

    const result = await phase;

    expect(result.aborted).toBe(true);
  },
  10000,
);

test(
  "a question-type planner message relays a dashboard pointer, not the raw JSON payload",
  async () => {
    const task = makeTask({ discord_thread_id: "thread-1" });
    mockMessageText = "exploring the repo before drafting a plan"; // non-PLAN_READY, keeps watchBatch polling

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(task.id, {
      from: "planner",
      type: "question",
      text: '{"questions":[{"question":"which?","header":"Q","options":[]}]}',
    });
    await Bun.sleep(1300); // outlast watchBatch's ~1s poll
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });
    await phase;

    expect(discordPosts.some((m) => m.includes("answer it on the dashboard"))).toBe(true);
    expect(discordPosts.some((m) => m.includes('"questions"'))).toBe(false);
  },
  10000,
);

test(
  "human approval resolves the phase and returns the planner's session id",
  async () => {
    const task = makeTask();

    const phase = runPlanningPhase(task);
    await Bun.sleep(50);
    pushMsg(task.id, { from: "human", text: "approved", type: "approve" });

    const result = await phase;

    expect(result.aborted).toBe(false);
    expect(result.planningSessionId).toMatch(/^planner-/);
  },
  10000,
);

test(
  "a crashed planner session triggers a session-ended checkpoint, not a silent hang",
  async () => {
    const task = makeTask({ discord_thread_id: "thread-1" });
    crashPlannerForTaskId = task.id;

    const phase = runPlanningPhase(task);
    // watchBatch polls every ~1s; wait a full cycle so it has definitely
    // already detected flags.sessionEnded and posted the session-ended
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
  "a PLAN_READY: post counts toward the round cap",
  async () => {
    const task = makeTask({ discord_thread_id: "thread-1" });
    mockMessageText = `${PLAN_READY_PREFIX} draft plan`;

    const phase = runPlanningPhase(task);
    await Bun.sleep(1300); // outlast watchBatch's ~1s poll so the checkpoint has fired
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });
    await phase;

    expect(discordPosts.some((m) => m.includes("Round 1 done"))).toBe(true);
  },
  10000,
);

test(
  "a non-PLAN_READY: post (e.g. an interview question) does not count toward the round cap",
  async () => {
    const task = makeTask({ discord_thread_id: "thread-1" });
    mockMessageText = "which quality attribute matters more here?"; // no PLAN_READY: prefix

    const phase = runPlanningPhase(task);
    await Bun.sleep(1300);
    pushMsg(task.id, { from: "human", text: "stop", type: "abort" });
    await phase;

    // The session still ends (single fake yield, no resume), so the
    // checkpoint that fires is session-ended, never a round-cap "Round 1
    // done" — proving the round cap didn't count the unprefixed message.
    expect(discordPosts.some((m) => m.includes("ended without reaching a decision"))).toBe(true);
    expect(discordPosts.some((m) => m.includes("Round 1 done"))).toBe(false);
  },
  10000,
);

test(
  "a stop mid-implementation aborts instead of reporting success",
  async () => {
    queryDelayMs = 1500; // outlast waitForCheckpointReply's ~1s poll so the abort wins the race
    const task = makeTask();

    const phase = runImplementationPhase(task, "planner-fixed-session");
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
    const task = makeTask();

    const result = await runImplementationPhase(task, "planner-fixed-session");

    expect(result.aborted).toBe(false);
    expect(result.summary).toContain("mock planner message");

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

test(
  "a 0-turn/$0 implementation result is classified transient, matching the real incident",
  async () => {
    const task = makeTask();
    forceResultForTaskId = task.id;
    forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };

    await expect(runImplementationPhase(task, "planner-fixed-session")).rejects.toThrow(TransientError);
  },
  10000,
);

test(
  "a genuine non-success implementation result throws a plain Error, not TransientError",
  async () => {
    const task = makeTask();
    forceResultForTaskId = task.id;
    forceResult = { subtype: "error_max_turns", num_turns: 5, total_cost_usd: 0.42 };

    const failure = runImplementationPhase(task, "planner-fixed-session");
    await expect(failure).rejects.toThrow("implementation stopped: error_max_turns after 5 turns, $0.42");

    const error = await failure.catch((e) => e);
    expect(error).not.toBeInstanceOf(TransientError);
  },
  10000,
);
