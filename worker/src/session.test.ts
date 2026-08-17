// Exercises session.ts's continuous-session driver — the generalized
// canUseTool prompt-and-wait gate (supersedes docs/adr/0021/0025's
// Write/Edit-absent-from-allowedTools + approve-signal framing), human
// abort via the streamed message feed, raw-message relay, and uniform
// result-subtype classification — without a real Claude session or
// sidecar. Mocks must be registered before importing session.js (Bun
// resolves mock.module() calls ahead of subsequent static imports).
import { test, expect, mock, beforeEach, afterAll } from "bun:test";


// --- fake sidecarClient.js ---

const pushedMessages: { seq: number; from: string; text: string; type?: string; replyTo?: number }[] = [];
const savedSessionIds: string[] = [];
const savedPermissionModes: string[] = [];
let nextSeq = 1;

// The human-message feed a real sidecar SSE stream would deliver — tests
// drive this directly instead of a real HTTP connection. onEntry is
// captured so a test can push into it at any point after runSession() starts.
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
  pushMessage: mock(async (from: string, text: string, type?: string, replyTo?: number) => {
    const seq = nextSeq++;
    pushedMessages.push({ seq, from, text, type, replyTo });
    return seq;
  }),
  saveSessionId: mock(async (id: string, model: string) => {
    savedSessionIds.push(id);
  }),
  savePermissionMode: mock(async (mode: string) => {
    savedPermissionModes.push(mode);
  }),
  getSession: mock(async () => ({ description: "test session", permissionMode: sessionPermissionMode, model: undefined })),
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

// A prompt from ANOTHER session, exactly as core writes it: from "session",
// type "discussion", sender encoded as the `[from session <id>]` text prefix
// (core/internal/coreserver/interagent.go). Every other test here pushes
// from: "human", so this half of the onEntry filter was never executed.
// Inherits pushHuman's hardcoded seq: 0 — fine for asserting on text, wrong
// for anything that touches the cursor.
function pushSession(text: string, fromSessionId = "peer-abc123def"): void {
  humanMessageHandler?.({ seq: 0, from: "session", text: `[from session ${fromSessionId}]\n\n${text}`, type: "discussion" });
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
let forceResult: ({ subtype: string; num_turns: number; total_cost_usd: number } & Record<string, unknown>) | null = null;
let crashOnRound: number | null = null;
let includeToolResult = false;
let capturedCanUseTool: ((toolName: string, input: unknown) => Promise<{ behavior: string; message?: string; updatedInput?: unknown }>) | null =
  null;
let interruptCalls = 0;
let setPermissionModeCalls: string[] = [];
// Non-null makes the fake SDK reject the set_permission_mode control request,
// which is what 0.3.233 really does for a mode its own gates refuse.
let setPermissionModeError: string | null = null;
// The mode the session ROW carries, i.e. what this pod launched in.
let sessionPermissionMode: string | undefined = undefined;
let queryOptions: Record<string, unknown> | null = null;
// Every value the fake session's generator actually pulled off the
// streamed prompt — lets a test assert a given piece of text really
// reached the InputQueue (i.e. became a real conversational turn), not
// just that some other side effect (like a permission denial) happened.
let consumedInputs: { message: { content: string } }[] = [];
// Arbitrary extra SDK messages the fake session yields ahead of its
// assistant message each round — lets a test drive a message shape the
// scripted happy path never produces (auth_status, tool_progress, a
// thinking block, …) through the real logSdkMessage.
let extraMessages: Record<string, unknown>[] = [];

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
        // The abort must interrupt this await, not merely stop the next
        // yield — otherwise the common "idle, waiting for the next streamed
        // input" case (an unprompted /stop) never resolves at all.
        //
        // It must also not leave a rejected promise nobody is holding. When
        // runSession throws on its own (a non-success result, a crash), its
        // finally block aborts while nothing is iterating this generator any
        // more; letting the rejection escape produces an unhandled rejection
        // that wedges the whole bun test file — no results, no per-test
        // timeout, just a process that never finishes. Catching it here keeps
        // the interrupt semantics and ends the generator cleanly.
        let next: IteratorResult<{ message: { content: string } }>;
        try {
          next = await Promise.race([
            iterator.next(),
            new Promise<never>((_, reject) => {
              if (abortController?.signal.aborted) reject(new Error("aborted"));
              abortController?.signal.addEventListener("abort", () => reject(new Error("aborted")));
            }),
          ]);
        } catch {
          return;
        }
        const { done, value } = next;
        if (done) return;
        consumedInputs.push(value as { message: { content: string } });
        round++;
        if (!sessionId) {
          sessionId = `agent-${crypto.randomUUID()}`;
          // Mirrors the real init's field set, not just session_id — the
          // relay forwards the whole environment and a thinner fake would
          // let a dropped field pass unnoticed.
          yield {
            type: "system",
            subtype: "init",
            session_id: sessionId,
            model: "claude-opus-4-8",
            permissionMode: "default",
            slash_commands: ["/compact"],
            skills: ["doubt-driven-development"],
            tools: ["Bash", "Read"],
            mcp_servers: [{ name: "agent-fleet-sidecar", status: "connected" }],
            cwd: "/workspace",
            claude_code_version: "2.1.0",
            agents: [],
            plugins: [],
            output_style: "default",
          };
        }
        if (round === crashOnRound) throw new Error("simulated session crash");

        for (const extra of extraMessages) yield extra;

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
        if (setPermissionModeError !== null) throw new Error(setPermissionModeError);
      }),
    });
  },
}));

const { runSession } = await import("./session.js");

beforeEach(() => {
  // canUseTool runs the tool input through rtk before it asks (session.ts) —
  // on a machine that actually has rtk installed that is a real subprocess,
  // and the fixed sleeps below start racing it. These tests are about the
  // gate, not the rewrite (rtkHook.test.ts covers that), so the rewrite is
  // switched off. Set per test, not once at import: `bun test` shares one
  // process, and rtkHook.test.ts needs it unset.
  process.env.RTK_DISABLED = "1";
  pushedMessages.length = 0;
  nextSeq = 1;
  savedSessionIds.length = 0;
  savedPermissionModes.length = 0;
  humanMessageHandler = null;
  mockMessageText = "mock agent message";
  forceResult = null;
  crashOnRound = null;
  includeToolResult = false;
  capturedCanUseTool = null;
  interruptCalls = 0;
  setPermissionModeCalls = [];
  setPermissionModeError = null;
  sessionPermissionMode = undefined;
  queryOptions = null;
  consumedInputs = [];
  extraMessages = [];
});

// makeTask is gone: runSession takes no argument now. A session's identity
// comes from env and its instruction from the transcript, so there is no task
// object to construct (docs/adr/0048).

test("tool wiring: default mode, no Write/Edit in allowedTools, canUseTool present, settingSources user+project", async () => {
  const promise = runSession();
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
  // The wildcard, not nine explicit entries. It matters that this list is
  // non-empty at all: the SDK's MCP tools return {behavior: "passthrough"},
  // which its evaluator turns into "ask", so dropping the allowlist would
  // make the agent need permission before it could ask for permission
  // (docs/adr/0048).
  expect(allowedTools).toContain("mcp__agent-fleet-sidecar__*");
  // The SDK's own built-in AskUserQuestion must stay out of context — only
  // the mcp__agent-fleet-sidecar__ one above renders as a real dashboard
  // question form; the native one falls through to the generic raw-JSON
  // PermissionCard with no way to deliver an answer.
  expect(queryOptions?.disallowedTools).toContain("AskUserQuestion");
  expect(typeof queryOptions?.canUseTool).toBe("function");
  // The provisioner-synced fleet-shared skills/context (docs/adr/0032) are
  // discovered natively via settingSources, not an explicit plugins: entry
  // — this is the actual "no session.ts change to add a skill" payoff.
  //
  // "project" is what gives a session the *target repo's* own CLAUDE.md and
  // .claude/skills/ (docs/adr/0049). "local" must stay out: it would load
  // .claude/settings.local.json, which is gitignored and so lands
  // unreviewed.
  expect(queryOptions?.settingSources).toEqual(["user", "project"]);
  expect(queryOptions?.settingSources).not.toContain("local");
  // docs/adr/0049's authority ceiling, injected per-session rather than shipped
  // in fleet-shared/settings.json (docs/adr/0052), and unconditional as of
  // docs/adr/0053 now that no mode is fixed at launch.
  expect((queryOptions?.settings as { permissions?: { ask?: string[] } })?.permissions?.ask).toContain("Bash(gh:*)");
  expect(queryOptions?.plugins).toBeUndefined();
}, 10000);

test("canUseTool posts a permission_request and blocks until a matching permission_response resolves it", async () => {
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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

  // ...and the resolution must exist outside this process's memory. Every
  // dashboard surface decides "is this still waiting on a human" from a
  // PERMISSION_RESPONSE replying to the request's seq; without one, a plan
  // answered by requesting changes stayed pending forever and its card
  // could only be dismissed by approving it.
  const request = pushedMessages.find((m) => m.type === "permission_request")!;
  const recorded = pushedMessages.find((m) => m.type === "permission_response");
  expect(recorded).toBeDefined();
  expect(recorded!.replyTo).toBe(request.seq);
  expect(JSON.parse(recorded!.text).behavior).toBe("deny");
  expect(JSON.parse(recorded!.text).message).toContain("split this into its own PR");
  // from "agent": core streams from=="human" entries back into this session
  // as new input, so recording it as human would feed the wrapper its own
  // bookkeeping.
  expect(recorded!.from).toBe("agent");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a peer session's message becomes a turn that names the sender and prompt_agent", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  pushSession("which of these four schemas do you want?");
  await Bun.sleep(20);

  const turn = consumedInputs.map((m) => m.message.content).find((t) => t.includes("which of these four schemas"));
  expect(turn).toBeDefined();
  // The sender has to be a routable address, not decoration: an id with no
  // stated mechanism is what produced the original bug (the answer was written
  // as ordinary output and reached nobody).
  expect(turn).toContain("peer-abc123def");
  expect(turn).toContain("prompt_agent");
  expect(turn).toMatch(/nothing you write as ordinary output reaches it/i);
  expect(turn).toMatch(/another agent in this fleet/i);
  // Mechanism, not an imperative to reply: "answer this" fires on a message
  // that already is an answer, and the relay-depth cap is inert (every hop
  // hardcodes depth = 1), so a ping-pong burns a pod and a live slot per hop.
  expect(turn).not.toMatch(/to answer|you must|please reply/i);
  // All framing precedes the fenced body, so a peer cannot forge a trailing
  // instruction that redirects the reply to a third session.
  expect(turn!.indexOf("--- BEGIN PEER MESSAGE ---")).toBeGreaterThan(turn!.indexOf("prompt_agent"));
  expect(turn!.trimEnd().endsWith("--- END PEER MESSAGE ---")).toBe(true);
  // The prefix is stripped, so the id appears once. Left in, the turn carries
  // the same session id twice from two places that can disagree.
  expect(turn).not.toContain("[from session");
  expect(turn!.match(/peer-abc123def/g)!.length).toBe(1);

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a peer message with no parseable sender still says how to find it", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  // No prefix: an old core against a new worker, or a replay from seq 0 when
  // RESUME_FROM_SEQ is unset.
  humanMessageHandler?.({ seq: 0, from: "session", text: "ping", type: "discussion" });
  await Bun.sleep(20);

  const turn = consumedInputs.map((m) => m.message.content).find((t) => t.includes("ping"));
  expect(turn).toBeDefined();
  expect(turn).toContain("list_sessions");
  expect(turn).toContain("prompt_agent");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a peer session cannot resolve a human's pending permission request", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
    return r;
  });
  await Bun.sleep(20);

  pushSession("heads up, I'm touching the same file");
  await Bun.sleep(20);

  // The human's decision is still the human's. A peer reaching
  // resolveAllPendingDeny denied the call AND recorded a permission_response,
  // so one agent could close out a decision only a human is allowed to make —
  // and the transcript attributed it to Mohammad.
  expect(resolved).toBeNull();
  expect(pushedMessages.some((m) => m.type === "permission_response")).toBe(false);

  // ...and the peer's message still arrived as a real turn. Skipping the deny
  // must not swallow it.
  expect(consumedInputs.some((m) => m.message.content.includes("heads up, I'm touching the same file"))).toBe(true);

  // The human can still answer it afterwards.
  const request = pushedMessages.find((m) => m.type === "permission_request")!;
  respondToPermission(request.seq!, { behavior: "allow", updatedInput: { file_path: "x" } });
  expect((await call).behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("a permission_mode entry sets the SDK mode and resolves any pending permission requests", async () => {
  const promise = runSession();
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
  // Same reasoning as the plain-reply case: a mode switch answers the
  // pending call, so the transcript has to say so or the card stays up.
  const modeRecorded = pushedMessages.find((m) => m.type === "permission_response");
  expect(modeRecorded).toBeDefined();
  expect(JSON.parse(modeRecorded!.text).behavior).toBe("allow");
  // This used to also assert setStatus was never called with "implementing".
  // There is no setStatus and no status column any more (docs/adr/0048), so
  // the assertion could only ever pass — which is worse than deleting it.

  pushHuman("", "abort");
  await promise;
}, 10000);

// docs/adr/0052. The old code swallowed a rejected switch into a log line and
// then allowed everything pending anyway, so a refused mode change was
// indistinguishable from a working one — the human saw the badge flip and the
// parked call go through, and the very next tool call prompted again.
test("a REJECTED permission_mode switch does not allow the pending call", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  setPermissionModeError = "Cannot set permission mode to auto: gate is not enabled";

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
  });
  await Bun.sleep(20);

  pushHuman("auto", "permission_mode");
  await Bun.sleep(20);

  expect(setPermissionModeCalls).toContain("auto");
  expect(resolved).toBeNull(); // still blocked — the mode never changed
  expect(pushedMessages.find((m) => m.type === "permission_response")).toBeUndefined();
  const told = pushedMessages.find((m) => m.type === "system" && m.text.includes("Could not switch to auto"));
  expect(told).toBeDefined();
  // core wrote the column before this pod saw the entry, so a refused switch
  // leaves the badge — and the next warm's launch mode — showing a mode the
  // SDK rejected. The worker writes the live one back.
  expect(savedPermissionModes).toContain("default");

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// A mode switch answers the ONE call it was aimed at. Several tool calls in
// the same assistant turn park side by side, and a blanket sweep meant
// approving a plan also approved whatever else was parked — a Bash nobody read.
test("a permission_mode switch answers the plan, not every other parked call", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  let bash: { behavior: string } | null = null;
  const bashCall = capturedCanUseTool!("Bash", { command: "rm -rf /tmp/x" }).then((r) => {
    bash = r as { behavior: string };
  });
  const planCall = capturedCanUseTool!("ExitPlanMode", { plan: "do the thing" });
  await Bun.sleep(20);

  pushHuman("auto", "permission_mode");
  const planResult = await planCall;

  expect(planResult.behavior).toBe("allow");
  expect(bash).toBeNull(); // still parked — a human has to answer it
  const responses = pushedMessages.filter((m) => m.type === "permission_response");
  expect(responses.length).toBe(1);

  pushHuman("", "abort");
  await promise;
  await bashCall;
}, 10000);

// docs/adr/0053. The fleet's own MCP tools are how the agent reaches a human at
// all, so prompting for them means needing permission before you can ask for
// permission — which is what shipped, live, for AskUserQuestion and
// send_message both. allowedTools is supposed to cover this and does not: SDK
// 0.3.233 turns every non-read-only MCP tool into an ask in plan mode, above
// the allow-rule lookup. Asserting on "no permission_request was pushed" rather
// than just the return value, because the transcript entry is what a human sees.
test("a fleet MCP tool is allowed without ever asking a human", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  const result = await capturedCanUseTool!("mcp__agent-fleet-sidecar__AskUserQuestion", {
    questions: [{ question: "which one?" }],
  });
  expect(result.behavior).toBe("allow");
  expect(findPermissionRequest("mcp__agent-fleet-sidecar__AskUserQuestion")).toBeUndefined();
  expect(pushedMessages.filter((m) => m.type === "permission_request")).toEqual([]);

  pushHuman("", "abort");
  await promise;
}, 10000);

// docs/adr/0053: `auto` means auto. Everything the SDK would have prompted for
// is answered here — including an ask-rule match like `gh`, which the SDK's own
// auto-mode classifier deliberately falls back to a human on.
test("auto mode allows without asking, except rm and sudo", async () => {
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  expect(queryOptions?.permissionMode).toBe("auto");
  // Still injected in auto — the rules stay the fleet's ceiling over a target
  // repo's own allow list; what changes is who answers them.
  expect((queryOptions?.settings as { permissions?: { ask?: string[] } })?.permissions?.ask).toContain("Bash(rm:*)");

  expect((await capturedCanUseTool!("Bash", { command: "gh pr create" })).behavior).toBe("allow");
  expect((await capturedCanUseTool!("Write", { file_path: "x" })).behavior).toBe("allow");
  expect(pushedMessages.filter((m) => m.type === "permission_request")).toEqual([]);

  let resolved: { behavior: string } | null = null;
  const dangerous = capturedCanUseTool!("Bash", { command: "rm -rf build" }).then((r) => {
    resolved = r as { behavior: string };
    return r;
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("Bash")).toBeDefined();

  pushHuman("", "abort");
  await promise;
  await dangerous;
}, 10000);

// The command is matched anywhere in the line, not just at its head — a
// `make build && sudo install` is the same decision as a bare `sudo install`.
test("auto mode still asks for a destructive command hidden mid-line", async () => {
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: unknown = null;
  const call = capturedCanUseTool!("Bash", { command: "make build && sudo install" }).then((r) => (resolved = r));
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("Bash")).toBeDefined();

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// Every mode switch is a live control request again — adr/0052's
// bypass-boundary relaunch went with the mode that needed it (docs/adr/0053).
test("switching to auto mid-session is a live control request, not a relaunch", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  const call = capturedCanUseTool!("Write", { file_path: "x" });
  pushHuman("auto", "permission_mode");
  const result = await call;

  expect(result.behavior).toBe("allow");
  expect(setPermissionModeCalls).toContain("auto");
  expect(pushedMessages.find((m) => m.type === "system" && m.text.includes("re-warm"))).toBeUndefined();

  // And the gate reads the new mode from here on, without a new pod.
  expect((await capturedCanUseTool!("Bash", { command: "go build ./..." })).behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("ExitPlanMode is gated the same as any other tool call — blocks until a permission_response arrives", async () => {
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
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
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("", "abort");
  const result = await promise;

  expect(result.aborted).toBe(true);
  expect(interruptCalls).toBeGreaterThan(0);
}, 10000);

test("/clear never reaches the SDK: it clears the saved session id and ends the pod so the next warm starts fresh", async () => {
  const promise = runSession();
  await Bun.sleep(20);
  // One real turn first, so the initial session id is captured and saved.
  pushHuman("hello", "discussion");
  await Bun.sleep(20);
  expect(savedSessionIds.length).toBe(1);
  expect(savedSessionIds[0]).not.toBe("");

  // `/clear` must NOT be pushed to the SDK (that throws in streaming mode).
  // It ends the session, and the LAST saved id is empty — next warm resumes
  // nothing.
  pushHuman("/clear", "discussion");
  const result = await promise;

  expect(result.aborted).toBe(true);
  expect(interruptCalls).toBeGreaterThan(0);
  expect(savedSessionIds.at(-1)).toBe("");
}, 10000);

// Every test that expects a round to HAPPEN has to push a message first.
// The input queue is deliberately never seeded (docs/adr/0048): a session
// with no message has no turn, which is what makes "optional first message"
// a resting state rather than a special case. Before the rewrite the task
// description was pushed automatically, so a bare runSession() produced
// round 1 on its own — these tests were written against that, and without
// the push they sit forever waiting for an input that never comes.
test("a crashed session propagates the error instead of hanging", async () => {
  crashOnRound = 1;
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start working", "discussion");
  await expect(promise).rejects.toThrow("simulated session crash");
}, 10000);

test("a successful round never ends the session on its own — no automated completion detection", async () => {
  mockMessageText = "done, opened the PR";
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("go", "discussion");
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
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("first message", "discussion");
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

test("a soft interrupt calls q.interrupt(), swallows the following non-success result, and the session survives for a later round", async () => {
  const promise = runSession();
  await Bun.sleep(20); // round 1: plain success, session now idle waiting for input

  pushHuman("stop the current turn", "interrupt");
  await Bun.sleep(20);
  expect(interruptCalls).toBe(1);

  // Round 2's result looks like a genuine failure — without the
  // interrupt's softInterrupted flag this would throw a plain Error (see
  // "a genuine non-success result throws a plain Error" below).
  forceResult = { subtype: "error_max_turns", num_turns: 5, total_cost_usd: 0.42 };
  pushHuman("continue please", "discussion");
  await Bun.sleep(20);

  // Round 3 proves the session is still alive and accepting input, not
  // silently dead — a plain break/return would leave `promise` unresolved
  // forever instead, so this also guards against a false-positive pass.
  forceResult = null;
  pushHuman("one more round", "discussion");
  await Bun.sleep(20);

  pushHuman("", "abort");
  const result = await promise;
  expect(result.aborted).toBe(true);
}, 10000);

// The two "classified transient" tests that used to sit here are gone with
// TransientError itself (docs/adr/0048). The distinction only ever existed to
// pick between two dispositions — fail the task, or leave it for core's
// reclaim to re-dispatch. Nothing re-dispatches a session now, so both
// branches lead to the same place: exit non-zero, Job Failed, pod_phase
// CRASHED with the message attached, and a human decides whether to warm it
// again. A test asserting which error class we picked would be testing a
// choice with no consequence.
test("a non-success result fails the run, carrying the SDK's own reason", async () => {
  forceResult = { subtype: "error_max_turns", num_turns: 5, total_cost_usd: 0.42 };
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start working", "discussion");

  // The message matters more than the type: it is what lands in last_error
  // and is the entirety of what a human has to go on from the dashboard.
  await expect(promise).rejects.toThrow("session stopped: error_max_turns after 5 turns, $0.42");
}, 10000);

test("a 0-turn/$0 result fails the same way, with no special case", async () => {
  forceResult = { subtype: "error_during_execution", num_turns: 0, total_cost_usd: 0 };
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start working", "discussion");

  await expect(promise).rejects.toThrow("session stopped: error_during_execution after 0 turns, $0");
}, 10000);

// Raw-relay behavior: every SDK message reaches pushMessage, not just
// assistant text blocks — tool_use, tool_result, and result summaries all
// get relayed too, each tagged with its own type (dashboard-only
// visibility; core's relay allowlist keeps them off Discord).
test("tool_use, tool_result, and result messages are all relayed, not just assistant text", async () => {
  includeToolResult = true;
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("go", "discussion");
  await Bun.sleep(20);
  pushHuman("", "abort");
  await promise;

  expect(pushedMessages.some((m) => m.type === "discussion" && m.text === "mock agent message")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "assistant")).toBe(true); // tool_use
  expect(pushedMessages.some((m) => m.type === "user")).toBe(true); // tool_result
  expect(pushedMessages.some((m) => m.type === "result")).toBe(true);
  expect(pushedMessages.some((m) => m.type === "system")).toBe(true); // session-init
}, 10000);

// Every SDK message kind that used to fall off the end of logSdkMessage
// unrelayed. These are the "why is the worker hung" answers — an expired
// token, a rate limit, and a long-running tool were all indistinguishable
// from silence before.
async function relayOnce(extras: Record<string, unknown>[]): Promise<void> {
  extraMessages = extras;
  const promise = runSession();
  await Bun.sleep(20);
  // A real turn has to happen for anything to be relayed at all. The session
  // does not start one on its own any more (docs/adr/0048 — an unseeded input
  // queue is what makes "session with no message" a valid resting state), so
  // relaying "one round" means pushing one message and letting it complete
  // before aborting.
  pushHuman("go", "discussion");
  await Bun.sleep(20);
  pushHuman("", "abort");
  await promise;
}

function systemPayloads(): Record<string, unknown>[] {
  return pushedMessages.filter((m) => m.type === "system").map((m) => JSON.parse(m.text) as Record<string, unknown>);
}

test("out-of-band SDK signals relay as system entries tagged with their sdk discriminant", async () => {
  await relayOnce([
    { type: "system", subtype: "status", status: "compacting", uuid: "u1", session_id: "s1" },
    { type: "system", subtype: "compact_boundary", compact_metadata: { trigger: "auto", pre_tokens: 142_000 } },
    { type: "system", subtype: "hook_response", hook_name: "guard", hook_event: "PreToolUse", exit_code: 2, stderr: "denied" },
    { type: "auth_status", isAuthenticating: false, error: "token expired" },
  ]);

  const bySdk = new Map(systemPayloads().map((p) => [p.sdk, p]));
  expect(bySdk.get("status")?.status).toBe("compacting");
  expect((bySdk.get("compact_boundary")?.compact_metadata as { pre_tokens: number }).pre_tokens).toBe(142_000);
  expect(bySdk.get("hook_response")?.exit_code).toBe(2);
  expect(bySdk.get("auth_status")?.error).toBe("token expired");
  // The envelope the transcript row already carries is not duplicated.
  expect(bySdk.get("status")).not.toHaveProperty("uuid");
  expect(bySdk.get("status")).not.toHaveProperty("session_id");
});

// SDK 0.3.233 added system subtypes that are re-emitted many times inside ONE
// event — thinking_tokens per thinking delta, task_progress per subagent poll,
// hook_progress per stdout chunk. Relaying them wrote a durable transcript row
// per FRAME, which drowned the dashboard feed in near-identical entries. Their
// terminal siblings (task_notification, hook_response, result) still relay.
test("per-frame SDK progress messages never reach the transcript", async () => {
  await relayOnce([
    { type: "system", subtype: "thinking_tokens", estimated_tokens: 120, estimated_tokens_delta: 120 },
    { type: "system", subtype: "thinking_tokens", estimated_tokens: 260, estimated_tokens_delta: 140 },
    { type: "system", subtype: "task_progress", task_id: "t1", description: "searching", usage: { total_tokens: 900 } },
    { type: "system", subtype: "hook_progress", hook_name: "rtk", hook_event: "PreToolUse", stdout: "chunk" },
    { type: "system", subtype: "control_request_progress", request_id: "r1", status: "started" },
    { type: "system", subtype: "session_state_changed", state: "running" },
    { type: "system", subtype: "task_notification", task_id: "t1", status: "completed", summary: "done" },
  ]);

  const relayed = systemPayloads().map((p) => p.sdk);
  for (const frame of ["thinking_tokens", "task_progress", "hook_progress", "control_request_progress", "session_state_changed"]) {
    expect(relayed).not.toContain(frame);
  }
  expect(relayed).toContain("task_notification");
});

test("an SDK-level assistant error relays even though the turn has no content block for it", async () => {
  await relayOnce([{ type: "assistant", error: "rate_limit", message: { content: [] } }]);

  expect(systemPayloads().some((p) => p.sdk === "assistant_error" && p.error === "rate_limit")).toBe(true);
});

test("thinking blocks relay under assistant, distinguishable from tool_use", async () => {
  await relayOnce([
    { type: "assistant", message: { content: [{ type: "thinking", thinking: "weighing two options" }] } },
  ]);

  const assistant = pushedMessages.filter((m) => m.type === "assistant").map((m) => JSON.parse(m.text) as Record<string, unknown>);
  expect(assistant.some((p) => p.kind === "thinking" && p.text === "weighing two options")).toBe(true);
  // The scripted round still emits a real tool_use, and it must stay parseable.
  expect(assistant.some((p) => p.kind === undefined && typeof p.tool === "string")).toBe(true);
});

// Shapes taken verbatim from a live 100s Bash call in the kind cluster.
// The ids really are synthetic and unique per emission — an earlier
// version of this relay throttled on tool_use_id and therefore never
// deduped anything, which no unit test could catch while it fed the same
// id three times. The SDK's own 30s gate is the throttle.
test("tool_progress relays every emission the SDK sends, synthetic ids and all", async () => {
  await relayOnce([
    { type: "tool_progress", tool_use_id: "bash-progress-30", tool_name: "Bash", elapsed_time_seconds: 32 },
    { type: "tool_progress", tool_use_id: "bash-progress-60", tool_name: "Bash", elapsed_time_seconds: 62 },
    { type: "tool_progress", tool_use_id: "bash-progress-90", tool_name: "Bash", elapsed_time_seconds: 92 },
  ]);

  const progress = systemPayloads().filter((p) => p.sdk === "tool_progress");
  expect(progress.map((p) => p.elapsed_time_seconds)).toEqual([32, 62, 92]);
  expect(progress.map((p) => p.tool_use_id)).toEqual(["bash-progress-30", "bash-progress-60", "bash-progress-90"]);
});

// parent_tool_use_id is the stream's only subagent attribution, so unlike
// uuid/session_id it must survive the envelope strip.
test("parent_tool_use_id survives relaying", async () => {
  await relayOnce([
    { type: "tool_progress", tool_use_id: "bash-progress-30", tool_name: "Bash", elapsed_time_seconds: 32, parent_tool_use_id: "toolu_parent" },
  ]);

  expect(systemPayloads().find((p) => p.sdk === "tool_progress")?.parent_tool_use_id).toBe("toolu_parent");
});

test("replayed user messages are dropped instead of double-posting", async () => {
  await relayOnce([
    {
      type: "user",
      isReplay: true,
      message: { content: [{ type: "tool_result", tool_use_id: "t9", is_error: false, content: "replayed output" }] },
    },
  ]);

  expect(pushedMessages.some((m) => m.text.includes("replayed output"))).toBe(false);
});

test("result carries the full usage/duration/error payload, not just turns and cost", async () => {
  forceResult = {
    subtype: "success",
    num_turns: 3,
    total_cost_usd: 0.42,
    duration_ms: 9000,
    duration_api_ms: 7000,
    is_error: false,
    usage: { input_tokens: 100, output_tokens: 20 },
    modelUsage: { "claude-opus-4-8": { inputTokens: 100 } },
    permission_denials: [],
    errors: [],
  };
  await relayOnce([]);

  const result = pushedMessages.filter((m) => m.type === "result").map((m) => JSON.parse(m.text) as Record<string, unknown>);
  expect(result.length).toBeGreaterThan(0);
  for (const key of ["subtype", "numTurns", "totalCostUsd", "durationMs", "durationApiMs", "isError", "usage", "modelUsage", "permissionDenials", "errors"]) {
    expect(result[0]).toHaveProperty(key);
  }
});

test("session init relays the full environment, not four of fourteen fields", async () => {
  await relayOnce([]);

  const init = systemPayloads().find((p) => p.model !== undefined);
  expect(init).toBeDefined();
  for (const key of ["model", "permissionMode", "slashCommands", "skills", "tools", "mcpServers", "cwd", "claudeCodeVersion", "agents", "plugins", "outputStyle"]) {
    expect(init).toHaveProperty(key);
  }
});

// Undoes this file's mock.module("./sidecarClient.js", ...) once this
// file's own tests are done — see the comment on that call for why this
// is required, not optional, in a shared-process test run.
afterAll(() => {
  mock.restore();
});

// The counterweight lives in session.ts's FLEET_ASK_RULES and nowhere else
// (docs/adr/0052, kept by 0053). A second copy in the settings file is the bad
// kind of redundancy: it applies at a different scope, cannot be varied per
// session, and diverges from the list canUseTool actually reasons about — with
// nothing anywhere reporting the disagreement.
test("fleet-shared/settings.json ships no permissions.ask block", async () => {
  const settings = await Bun.file(new URL("../../fleet-shared/settings.json", import.meta.url)).json();
  expect(settings.permissions.ask).toBeUndefined();
  expect(settings.permissions.allow.length).toBeGreaterThan(0);
});
