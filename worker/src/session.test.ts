// Exercises session.ts's continuous-session driver — the generalized
// canUseTool prompt-and-wait gate (supersedes docs/adr/0021/0025's
// Write/Edit-absent-from-allowedTools + approve-signal framing), human
// abort via the streamed message feed, raw-message relay, and uniform
// result-subtype classification — without a real Claude session or
// sidecar. Mocks must be registered before importing session.js (Bun
// resolves mock.module() calls ahead of subsequent static imports).
import { test, expect, mock, beforeEach, afterAll } from "bun:test";

// --- fake sidecarClient.js ---

const pushedMessages: { seq: number; from: string; text: string; type?: string }[] = [];
const savedSessionIds: string[] = [];
const statusUpdates: string[] = [];
let nextSeq = 1;

// The human-message feed a real sidecar SSE stream would deliver — tests
// drive this directly instead of a real HTTP connection. onEntry is
// captured so a test can push into it at any point after runTask() starts.
let humanMessageHandler: ((entry: { seq: number; from: string; text: string; type?: string; replyTo?: number }) => void | Promise<void>) | null =
  null;

// mock.module() replaces this specifier in Bun's module registry for the
// rest of the whole `bun test` process, not just this file — bun test
// runs every file in one shared process (sidecarClient.ts's own comment
// documents this same hazard for a different reason). Left unrestored,
// sidecarClient.test.ts (which needs the REAL implementation against a
// real local HTTP server) can end up importing this fake instead — its
// streamHumanMessages here only resolves on external abort and never
// calls onEntry on its own, which reproduced as every sidecarClient.test.ts
// case hanging for the full 10s timeout in CI (order-dependent, so it
// didn't reproduce locally where file execution order happened to differ).
mock.module("./sidecarClient.js", () => ({
  pushMessage: mock(async (from: string, text: string, type?: string) => {
    const seq = nextSeq++;
    pushedMessages.push({ seq, from, text, type });
    return seq;
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

function pushHuman(text: string, type?: string, replyTo?: number): void {
  humanMessageHandler?.({ seq: 0, from: "human", text, type, replyTo });
}

// Finds the permission_request pushed for a given tool (there can be more
// than one pending at once) and answers it via a permission_response
// correlated by that request's own seq.
function findPermissionRequest(tool: string) {
  return pushedMessages.find((m) => m.type === "permission_request" && JSON.parse(m.text).tool === tool);
}

function respondToPermission(seq: number, decision: { behavior: "allow" | "deny"; updatedInput?: unknown; message?: string }): void {
  pushHuman(JSON.stringify(decision), "permission_response", seq);
}

// --- fake @anthropic-ai/claude-agent-sdk ---

// One "round" per message the fake session receives via the streamed
// prompt — each round yields system init (first round only), an assistant
// message (optionally followed by a tool_result user message), then a
// result, then waits for the next input. Mirrors the real SDK's
// streaming-input behavior confirmed in this session's own Phase 0 spike:
// the same Query object keeps accepting input after interrupt().
let mockMessageText = "mock agent message";
let forceResult: { subtype: string; num_turns: number; total_cost_usd: number } | null = null;
let crashOnRound: number | null = null;
let includeToolResult = false;
let capturedCanUseTool: ((toolName: string, input: unknown) => Promise<{ behavior: string; message?: string; updatedInput?: unknown }>) | null =
  null;
let interruptCalls = 0;
let setPermissionModeCalls: string[] = [];
let queryOptions: Record<string, unknown> | null = null;
// Every value the fake session's generator actually pulled off the
// streamed prompt — lets a test assert a given piece of text really
// reached the InputQueue (i.e. became a real conversational turn), not
// just that some other side effect (like a permission denial) happened.
let consumedInputs: { message: { content: string } }[] = [];

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
        const { done, value } = next;
        if (done) return;
        consumedInputs.push(value as { message: { content: string } });
        round++;
        if (!sessionId) {
          sessionId = `agent-${crypto.randomUUID()}`;
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

const { runTask, TransientError } = await import("./session.js");

beforeEach(() => {
  pushedMessages.length = 0;
  nextSeq = 1;
  savedSessionIds.length = 0;
  statusUpdates.length = 0;
  humanMessageHandler = null;
  mockMessageText = "mock agent message";
  forceResult = null;
  crashOnRound = null;
  includeToolResult = false;
  capturedCanUseTool = null;
  interruptCalls = 0;
  setPermissionModeCalls = [];
  queryOptions = null;
  consumedInputs = [];
});

function makeTask(overrides: Partial<{ id: string; repo: string; description: string; leaseId: string; baseBranch: string; guidance: string }> = {}) {
  return {
    id: crypto.randomUUID(),
    repo: "dream-analyst",
    description: "test task",
    leaseId: "lease-1",
    baseBranch: "main",
    guidance: "",
    ...overrides,
  };
}

test("tool wiring: default mode, no Write/Edit in allowedTools, canUseTool present, local skills plugin", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions).not.toBeNull();
  expect(queryOptions?.permissionMode).toBe("default");
  // No RESUME_SESSION_ID env in this process — a fresh task, not a warmed
  // one (RESUME_SESSION_ID is read once at module load, like MODEL/
  // MAX_TURNS, so the positive "resume passed through" case isn't
  // separately unit-testable here; covered instead by
  // provisioner/internal/k8s/k8s_test.go's TestCreateWorkerPod_ResumeSession
  // for the env-var plumbing, and manually via kind-local for the full path).
  expect(queryOptions?.resume).toBeUndefined();
  const allowedTools = queryOptions?.allowedTools as string[];
  expect(allowedTools).not.toContain("Write");
  expect(allowedTools).not.toContain("Edit");
  expect(allowedTools).not.toContain("Bash");
  expect(allowedTools).toContain("Task");
  expect(allowedTools).toContain("mcp__agent-fleet-sidecar__AskUserQuestion");
  expect(typeof queryOptions?.canUseTool).toBe("function");
  const plugins = queryOptions?.plugins as Array<{ type: string; path: string }>;
  expect(plugins.some((p) => p.type === "local" && p.path.includes("agent-fleet-planning"))).toBe(true);
}, 10000);

test("canUseTool posts a permission_request and blocks until a matching permission_response resolves it", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
    return r;
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull(); // still blocked, no auto-allow

  const req = findPermissionRequest("Write");
  expect(req).toBeDefined();
  expect(JSON.parse(req!.text)).toEqual({ tool: "Write", input: { file_path: "x" } });

  respondToPermission(req!.seq, { behavior: "allow" });
  const result = await call;
  expect(result.behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a denied permission_response carries the human's reason through to the tool result", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const call = capturedCanUseTool!("Bash", { command: "rm -rf /" });
  await Bun.sleep(20);
  const req = findPermissionRequest("Bash");
  respondToPermission(req!.seq, { behavior: "deny", message: "absolutely not" });

  const result = await call;
  expect(result.behavior).toBe("deny");
  expect(result.message).toBe("absolutely not");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("parallel tool calls each get their own correlated permission_request, resolved independently", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  let writeResolved: { behavior: string } | null = null;
  let editResolved: { behavior: string } | null = null;
  const writeCall = capturedCanUseTool!("Write", { file_path: "a" }).then((r) => {
    writeResolved = r as { behavior: string };
  });
  const editCall = capturedCanUseTool!("Edit", { file_path: "b" }).then((r) => {
    editResolved = r as { behavior: string };
  });
  await Bun.sleep(20);

  const writeReq = findPermissionRequest("Write")!;
  const editReq = findPermissionRequest("Edit")!;
  expect(writeReq.seq).not.toBe(editReq.seq);

  // Resolve Edit first — Write must stay pending.
  respondToPermission(editReq.seq, { behavior: "allow" });
  await editCall;
  expect(editResolved!.behavior).toBe("allow");
  expect(writeResolved).toBeNull();

  respondToPermission(writeReq.seq, { behavior: "deny", message: "no" });
  await writeCall;
  expect(writeResolved!.behavior).toBe("deny");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("an answer-type human entry is never misread as a permission decision", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  // A human's chosen option label could itself contain JSON-ish text — an
  // "answer"-type entry must never be treated as resolving anything here,
  // only a structured type:"permission_response"/"permission_mode" does.
  pushHuman('{"answers":{"q":"allow it"}}', "answer");
  await Bun.sleep(20);
  expect(setPermissionModeCalls.length).toBe(0);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("free text alone never resolves a pending permission request or triggers abort", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("I allowed a similar edit yesterday, let's stop and think about this first", "discussion");
  await Bun.sleep(20);
  expect(setPermissionModeCalls.length).toBe(0);
  expect(interruptCalls).toBe(0);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("a plain reply while a permission request is pending denies it with the reply as feedback", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const call = capturedCanUseTool!("Write", { file_path: "x" });
  await Bun.sleep(20);

  pushHuman("actually, split this into its own PR", "discussion");
  const result = await call;
  expect(result.behavior).toBe("deny");
  expect((result as { message?: string }).message).toContain("split this into its own PR");

  // The same reply must also reach the agent as a real conversational
  // turn, not just live embedded inside the denial reason — otherwise
  // there's nothing for the agent to actually respond to.
  await Bun.sleep(20);
  expect(consumedInputs.some((m) => m.message.content === "actually, split this into its own PR")).toBe(true);

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a permission_mode entry sets the SDK mode and resolves any pending permission requests", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull();

  pushHuman("acceptEdits", "permission_mode");
  await call;

  expect(resolved!.behavior).toBe("allow");
  expect(setPermissionModeCalls).toContain("acceptEdits");
  // No fleet-imposed phase left to report — status stays whatever it was.
  expect(statusUpdates).not.toContain("implementing");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("ExitPlanMode is gated the same as any other tool call — blocks until a permission_response arrives", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  let resolved: { behavior: string; updatedInput?: unknown } | null = null;
  const planCall = capturedCanUseTool!("ExitPlanMode", { plan: "do the thing" }).then((r) => {
    resolved = r as { behavior: string; updatedInput?: unknown };
    return r;
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull(); // still blocked, no canned SDK-style auto-allow

  const req = findPermissionRequest("ExitPlanMode")!;
  respondToPermission(req.seq, { behavior: "allow" });
  const result = await planCall;
  expect(result.behavior).toBe("allow");
  expect((result as { updatedInput?: unknown }).updatedInput).toEqual({ plan: "do the thing" });

  pushHuman("", "abort");
  await promise;
}, 10000);

test("ExitPlanMode blocks until a permission_mode selection arrives, then allows", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const planCall = capturedCanUseTool!("ExitPlanMode", { plan: "do the thing" });
  await Bun.sleep(20);

  pushHuman("acceptEdits", "permission_mode");
  const result = await planCall;
  expect(result.behavior).toBe("allow");
  expect(setPermissionModeCalls).toContain("acceptEdits");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("abort resolves every pending permission request instead of leaving them hanging", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const planCall = capturedCanUseTool!("ExitPlanMode", { plan: "do the thing" });
  const writeCall = capturedCanUseTool!("Write", { file_path: "x" });
  await Bun.sleep(20);

  pushHuman("", "abort");
  const [planResult, writeResult] = await Promise.all([planCall, writeCall]);
  expect(planResult.behavior).toBe("deny");
  expect(writeResult.behavior).toBe("deny");

  await promise;
}, 10000);

test("Bash is gated by canUseTool, not silently allowed via allowedTools", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);

  const allowedTools = queryOptions?.allowedTools as string[];
  expect(allowedTools).not.toContain("Bash");

  const call = capturedCanUseTool!("Bash", { command: "git status" });
  await Bun.sleep(20);
  expect(findPermissionRequest("Bash")).toBeDefined();
  respondToPermission(findPermissionRequest("Bash")!.seq, { behavior: "allow" });
  await call;

  pushHuman("", "abort");
  await promise;
}, 10000);

test("abort before anything is answered ends the task as aborted", async () => {
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

test("a successful round never ends the session on its own — no automated completion detection", async () => {
  const task = makeTask();
  mockMessageText = "done, opened the PR";
  const promise = runTask(task);
  await Bun.sleep(20);

  // The session already ran one successful round but must still be
  // running, waiting for the next input — nothing in the agent's own text
  // ends it, no matter what it says.
  expect(savedSessionIds.length).toBe(1);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("the session keeps consuming successful rounds indefinitely until a human stops it", async () => {
  const task = makeTask();
  const promise = runTask(task);
  await Bun.sleep(20);
  expect(savedSessionIds.length).toBe(1);

  // Several more rounds, each a plain discussion reply feeding the input
  // queue — the loop must keep going through all of them, exactly like an
  // idle terminal session picking up each new message.
  pushHuman("keep going", "discussion");
  await Bun.sleep(20);
  pushHuman("still keep going", "discussion");
  await Bun.sleep(20);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

test("a 0-turn/$0 result is classified transient", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };
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

test("a non-success 0-turn/$0 result is classified transient regardless of when it happens", async () => {
  const task = makeTask();
  forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };
  const promise = runTask(task);

  await expect(promise).rejects.toThrow(TransientError);
}, 10000);

// Raw-relay behavior: every SDK message reaches pushMessage, not just
// assistant text blocks — tool_use, tool_result, and result summaries all
// get relayed too, each tagged with its own type (dashboard-only
// visibility; core's relay allowlist keeps them off Discord).
test("tool_use, tool_result, and result messages are all relayed, not just assistant text", async () => {
  const task = makeTask();
  includeToolResult = true;
  const promise = runTask(task);
  await Bun.sleep(20);
  pushHuman("", "abort");
  await promise;

  expect(pushedMessages.some((m) => m.type === "discussion" && m.text === "mock agent message")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "assistant")).toBe(true); // tool_use
  expect(pushedMessages.some((m) => m.type === "user")).toBe(true); // tool_result
  expect(pushedMessages.some((m) => m.type === "result")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "system")).toBe(true); // session-init
}, 10000);

// Undoes this file's mock.module("./sidecarClient.js", ...) once this
// file's own tests are done — see the comment on that call for why this
// is required, not optional, in a shared-process test run.
afterAll(() => {
  mock.restore();
});
