// Exercises planning.ts's continuous-session driver (docs/adr/0021,
// reliability-findings.md #0) — the canUseTool gate, human approval/abort
// via the streamed message feed (structured signals only, no free-text
// regex), raw-message relay, and uniform result-subtype classification —
// without a real Claude session or sidecar. Mocks must be registered
// before importing planning.js (Bun resolves mock.module() calls ahead of
// subsequent static imports).
import { test, expect, mock, beforeEach } from "bun:test";

// --- fake sidecarClient.js ---

const pushedMessages: { from: string; text: string; type?: string }[] = [];
const savedSessionIds: string[] = [];
const statusUpdates: string[] = [];

// The human-message feed a real sidecar SSE stream would deliver — tests
// drive this directly instead of a real HTTP connection. onEntry is
// captured so a test can push into it at any point after runTask() starts.
let humanMessageHandler: ((entry: { seq: number; from: string; text: string; type?: string }) => void | Promise<void>) | null = null;

mock.module("./sidecarClient.js", () => ({
  pushMessage: mock(async (from: string, text: string, type?: string) => {
    pushedMessages.push({ from, text, type });
  }),
  saveSessionId: mock(async (id: string) => {
    savedSessionIds.push(id);
  }),
  setStatus: mock(async (status: string) => {
    statusUpdates.push(status);
  }),
  streamHumanMessages: mock(async (onEntry: typeof humanMessageHandler, signal: AbortSignal) => {
    humanMessageHandler = onEntry;
    // Resolves only when the caller aborts — mirrors the real SSE stream's
    // "runs until aborted" contract (worker/src/sidecarClient.ts).
    await new Promise<void>((resolve) => {
      signal.addEventListener("abort", () => resolve());
    });
  }),
}));

function pushHuman(text: string, type?: string): void {
  humanMessageHandler?.({ seq: 0, from: "human", text, type });
}

// --- fake @anthropic-ai/claude-agent-sdk ---

// One "round" per message the fake session receives via the streamed
// prompt — each round yields system init (first round only), an assistant
// message (optionally followed by a tool_result user message), then a
// result, then waits for the next input. Mirrors the real SDK's
// streaming-input behavior confirmed in this session's own Phase 0 spike:
// the same Query object keeps accepting input after interrupt().
let mockMessageText = "mock planner message";
let forceResult: { subtype: string; num_turns: number; total_cost_usd: number } | null = null;
let crashOnRound: number | null = null;
let includeToolResult = false;
let capturedCanUseTool: ((toolName: string, input: unknown) => Promise<{ behavior: string; message?: string }>) | null = null;
let interruptCalls = 0;
let setPermissionModeCalls: string[] = [];
let queryOptions: Record<string, unknown> | null = null;

mock.module("@anthropic-ai/claude-agent-sdk", () => ({
  query: ({ prompt, options }: { prompt: AsyncIterable<{ message: { content: string } }>; options: Record<string, unknown> }) => {
    queryOptions = options;
    capturedCanUseTool = options.canUseTool as typeof capturedCanUseTool;
    const iterator = prompt[Symbol.asyncIterator]();
    const abortController = options.abortController as AbortController;
    let round = 0;
    let sessionId = "";

    async function* generate() {
      for (;;) {
        // Mirrors a real aborted session: abortController.abort() rejects
        // whatever the generator is currently awaiting, it doesn't just
        // stop yielding. Without this, the "idle, waiting for the next
        // streamed input" case (the common shape of an unprompted /stop)
        // never resolves at all.
        const next = await Promise.race([
          iterator.next(),
          new Promise<never>((_, reject) => {
            if (abortController?.signal.aborted) reject(new Error("aborted"));
            abortController?.signal.addEventListener("abort", () => reject(new Error("aborted")));
          }),
        ]);
        const { done } = next;
        if (done) return;
        round++;
        if (!sessionId) {
          sessionId = `planner-${crypto.randomUUID()}`;
          yield { type: "system", subtype: "init", session_id: sessionId };
        }
        if (round === crashOnRound) throw new Error("simulated session crash");

        yield {
          type: "assistant",
          message: {
            content: [
              {
                type: "tool_use",
                name: "mcp__agent-fleet-sidecar__send_message",
                input: { text: mockMessageText },
              },
              { type: "text", text: mockMessageText },
            ],
          },
        };
        if (includeToolResult) {
          yield {
            type: "user",
            message: { content: [{ type: "tool_result", is_error: false, content: "tool output" }] },
          };
        }
        if (forceResult) {
          yield { type: "result", ...forceResult };
          continue;
        }
        yield { type: "result", subtype: "success", num_turns: 1, total_cost_usd: 0.01 };
      }
    }

    const gen = generate();
    return Object.assign(gen, {
      interrupt: mock(async () => {
        interruptCalls++;
      }),
      setPermissionMode: mock(async (mode: string) => {
        setPermissionModeCalls.push(mode);
      }),
    });
  },
}));

const { runTask, TransientError } = await import("./planning.js");

beforeEach(() => {
  pushedMessages.length = 0;
  savedSessionIds.length = 0;
  statusUpdates.length = 0;
  humanMessageHandler = null;
  mockMessageText = "mock planner message";
  forceResult = null;
  crashOnRound = null;
  includeToolResult = false;
  capturedCanUseTool = null;
  interruptCalls = 0;
  setPermissionModeCalls = [];
  queryOptions = null;
});

function makeTask(overrides: Partial<{ id: string; repo: string; description: string; leaseId: string; baseBranch: string }> = {}) {
  return {
    id: crypto.randomUUID(),
    repo: "dream-analyst",
    description: "test task",
    leaseId: "lease-1",
    baseBranch: "main",
    ...overrides,
  };
}

test("tool wiring: plan mode, no Write/Edit in allowedTools, canUseTool present, local skills plugin", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions).not.toBeNull();
  expect(queryOptions?.permissionMode).toBe("plan");
  const allowedTools = queryOptions?.allowedTools as string[];
  expect(allowedTools).not.toContain("Write");
  expect(allowedTools).not.toContain("Edit");
  expect(allowedTools).toContain("Task");
  expect(allowedTools).toContain("mcp__agent-fleet-sidecar__AskUserQuestion");
  expect(typeof queryOptions?.canUseTool).toBe("function");
  const plugins = queryOptions?.plugins as Array<{ type: string; path: string }>;
  expect(plugins.some((p) => p.type === "local" && p.path.includes("agent-fleet-planning"))).toBe(true);
}, 10000);

test("canUseTool denies Write/Edit before approval, allows after", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const beforeApproval = await capturedCanUseTool!("Write", { file_path: "x" });
  expect(beforeApproval.behavior).toBe("deny");

  pushHuman("", "approve");
  await Bun.sleep(20);
  const afterApproval = await capturedCanUseTool!("Write", { file_path: "x" });
  expect(afterApproval.behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("an answer-type human entry is never misread as approval/abort", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  // A human's chosen option label could itself contain a word that used to
  // trip the deleted free-text isApproval/isAbort fallback (docs/adr/0018)
  // — an "answer"-type entry must never resolve the phase on its own, only
  // a structured type:"approve"/"abort" does now (reliability-findings.md
  // #0/#3).
  pushHuman('{"answers":{"q":"approved, ship it"}}', "answer");
  await Bun.sleep(20);
  expect(setPermissionModeCalls.length).toBe(0);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("free text alone never triggers approval or abort — only structured signals do", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  // Plain conversational text containing words the old regex fallback
  // matched ("approved", "stop") must do nothing now that isApproval/
  // isAbort are deleted entirely (reliability-findings.md #0/#3).
  pushHuman("I approved a similar PR yesterday, let's stop and think about this first", "discussion");
  await Bun.sleep(20);
  expect(setPermissionModeCalls.length).toBe(0);
  expect(interruptCalls).toBe(0);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("human approval flips the session to implementation and reports status", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("", "approve");
  await Bun.sleep(20);

  expect(setPermissionModeCalls).toContain("default");
  expect(statusUpdates).toContain("implementing");
  expect(savedSessionIds.length).toBe(1);
  expect(savedSessionIds[0]).toMatch(/^planner-/);

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a permission_mode entry sets the SDK mode, unlocks canUseTool, and reports status", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const beforeMode = await capturedCanUseTool!("Write", { file_path: "x" });
  expect(beforeMode.behavior).toBe("deny");

  pushHuman("acceptEdits", "permission_mode");
  await Bun.sleep(20);

  expect(setPermissionModeCalls).toContain("acceptEdits");
  expect(statusUpdates).toContain("implementing");
  const afterMode = await capturedCanUseTool!("Write", { file_path: "x" });
  expect(afterMode.behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("abort before approval ends the task as aborted", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("", "abort");
  const result = await promise;

  expect(result.aborted).toBe(true);
  expect(interruptCalls).toBeGreaterThan(0);
}, 10000);

test("a crashed session propagates the error instead of hanging", async () => {
  const task = makeTask();
  crashOnRound = 1;
  await expect(runTask(task)).rejects.toThrow("simulated session crash");
}, 10000);

test("implementation completes and returns the session's final text", async () => {
  const task = makeTask();
  // Approval immediately pushes the fixed post-approval input as the very
  // next round (runTask breaks on the first post-approval success result)
  // — the desired final text has to be in place *before* approving.
  mockMessageText = "PR_READY: did the thing";
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("", "approve");

  const result = await promise;
  expect(result.aborted).toBe(false);
  expect(result.summary).toContain("PR_READY: did the thing");
}, 10000);

test("a 0-turn/$0 result is classified transient", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };
  // Uniform handling (reliability-findings.md #8) means this now throws on
  // the very first round, before any approval — no pushHuman needed, and
  // attaching the rejection handler immediately avoids an unhandled-
  // rejection race against a Bun.sleep that used to give approval time to
  // land first.
  await expect(runTask(task)).rejects.toThrow(TransientError);
}, 10000);

test("a genuine non-success result throws a plain Error", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_max_turns", num_turns: 5, total_cost_usd: 0.42 };
  const promise = runTask(task);

  await expect(promise).rejects.toThrow("session stopped: error_max_turns after 5 turns, $0.42");
  const error = await promise.catch((e) => e);
  expect(error).not.toBeInstanceOf(TransientError);
}, 10000);

// Closes reliability-findings.md #8's actual gap: before #0, only the
// post-approval branch ever checked msg.subtype, so a non-success result
// during what used to be the planning phase went unhandled. Now the check
// is uniform — no approved/!approved branch split left to fall through.
test("a non-success result before approval is classified the same as after approval", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_max_turns", num_turns: 3, total_cost_usd: 0.1 };
  const promise = runTask(task);

  await expect(promise).rejects.toThrow("session stopped: error_max_turns after 3 turns, $0.1");
  const error = await promise.catch((e) => e);
  expect(error).not.toBeInstanceOf(TransientError);
}, 10000);

test("a non-success 0-turn/$0 result before approval is classified transient", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };
  const promise = runTask(task);

  await expect(promise).rejects.toThrow(TransientError);
}, 10000);

// Raw-relay behavior (reliability-findings.md #0): every SDK message
// reaches pushMessage now, not just assistant text blocks — tool_use,
// tool_result, and result summaries all get relayed too, each tagged with
// its own type (dashboard-only visibility; core's relay allowlist keeps
// them off Discord).
test("tool_use, tool_result, and result messages are all relayed, not just assistant text", async () => {
  const task = makeTask();
  includeToolResult = true;
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("", "abort");
  await promise;

  expect(pushedMessages.some((m) => m.type === "discussion" && m.text === "mock planner message")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "assistant")).toBe(true); // tool_use
  expect(pushedMessages.some((m) => m.type === "user")).toBe(true); // tool_result
  expect(pushedMessages.some((m) => m.type === "result")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "system")).toBe(true); // session-init
}, 10000);
