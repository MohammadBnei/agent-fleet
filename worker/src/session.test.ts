// Exercises session.ts's continuous-session driver — the generalized
// canUseTool prompt-and-wait gate (supersedes docs/adr/0021/0025's
// Write/Edit-absent-from-allowedTools + approve-signal framing), human
// abort via the streamed message feed, raw-message relay, and uniform
// result-subtype classification — without a real Claude session or
// sidecar. Mocks must be registered before importing session.js (Bun
// resolves mock.module() calls ahead of subsequent static imports).
import { mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test, expect, mock, beforeEach, afterAll, describe } from "bun:test";


// --- fake sidecarClient.js ---

const pushedMessages: { seq: number; from: string; text: string; type?: string; replyTo?: number }[] = [];
const savedSessionIds: string[] = [];
const savedModels: string[] = [];
// The row's model and effort as GET /task returns them (docs/adr/0077).
let sessionModel: string | undefined = undefined;
let sessionEffort: string | undefined = undefined;
const savedPermissionModes: string[] = [];
let nextSeq = 1;
// A definition session's spec as GET /task returns it; null = a repo session.
let sessionAgentSpec: Record<string, unknown> | null = null;
// The agentSpecSha the worker passed when it opened the human-message feed.
let streamedSpecSha: string | undefined = undefined;

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
    savedModels.push(model);
  }),
  savePermissionMode: mock(async (mode: string) => {
    savedPermissionModes.push(mode);
  }),
  getSession: mock(async () => ({
    description: "test session",
    permissionMode: sessionPermissionMode,
    model: sessionModel,
    effort: sessionEffort,
    ...(sessionAgentSpec ? { agentSpec: sessionAgentSpec, agentSpecSha: "spec-sha" } : {}),
  })),
  streamHumanMessages: mock(async (onEntry: typeof humanMessageHandler, signal: AbortSignal, _startSeq?: number, agentSpecSha?: string) => {
    humanMessageHandler = onEntry;
    streamedSpecSha = agentSpecSha;
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
// Forces the fake init to report a mode other than the one launched in — the
// SDK refusing "auto" at boot.
let initPermissionMode: string | undefined = undefined;
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
// Yielded before init on the first round, as the SDK's SessionStart/Setup
// hook messages can be.
let preInitMessages: Record<string, unknown>[] = [];

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
          for (const pre of preInitMessages) yield { ...pre, session_id: sessionId };
          // Mirrors the real init's field set, not just session_id — the
          // relay forwards the whole environment and a thinner fake would
          // let a dropped field pass unnoticed.
          yield {
            type: "system",
            subtype: "init",
            session_id: sessionId,
            model: "claude-opus-4-8",
            permissionMode: initPermissionMode ?? (queryOptions?.permissionMode as string | undefined) ?? "default",
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
      getContextUsage: mock(async () => ({
        memoryFiles: [{ path: "/genome/.claude/rules/90-fleet.md", type: "Project", tokens: 10 }],
        skills: { skillFrontmatter: [{ name: "journal", source: "projectSettings", tokens: 1 }, { name: "journal", source: "userSettings", tokens: 1 }] },
      })),
    });
  },
}));

// session.ts snapshots its env at module load, deliberately (RESUME_SESSION_ID,
// MODEL, MAX_TURNS and the rest are read once at :13-36). This file therefore
// inherits whatever the process it runs in happens to export — and a worker pod
// running a RESUMED session exports RESUME_SESSION_ID, so `bun test` from any
// warmed session failed the tool-wiring case below with a real session id where
// it asserted undefined. Nothing was wrong with the code; the test was reading
// its environment. Clear the lot before the import so the module takes its
// documented defaults wherever this runs.
for (const key of [
  "RESUME_SESSION_ID",
  "RESUME_FROM_SEQ",
  "SESSION_ID",
  "CLAUDE_MODEL",
  "MAX_TURNS",
  "WORKTREE_PATH",
  "SIDECAR_MCP_ADDR",
]) {
  delete process.env[key];
}

const { FLEET_TRAILER } = await import("./gitHooks.js");
const { runSession, sumModelUsage, formatAnswerTurn, ANSWER_NO_REASK, SIDECAR_MCP_TIMEOUT_MS, matchesToolRule, definitionDecision, gitArgsAreInert, sdkMode } = await import("./session.js");

beforeEach(() => {
  pushedMessages.length = 0;
  nextSeq = 1;
  savedSessionIds.length = 0;
  savedModels.length = 0;
  sessionModel = undefined;
  sessionEffort = undefined;
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
  initPermissionMode = undefined;
  sessionPermissionMode = undefined;
  queryOptions = null;
  consumedInputs = [];
  extraMessages = [];
  preInitMessages = [];
  sessionAgentSpec = null;
  streamedSpecSha = undefined;
});

// makeTask is gone: runSession takes no argument now. A session's identity
// comes from env and its instruction from the transcript, so there is no task
// object to construct (docs/adr/0048).

// docs/adr/0064: a definition pod (FLEET_DEFINITION, set by the provisioner's
// definition profile) closes the native cross-session channel both ways: it
// never touches core, so core's definition allowlist cannot bound it.
test("definition pod: native cross-session channel refused, SendMessage/ListAgents disallowed", async () => {
  process.env.FLEET_DEFINITION = "dev-dream-analyst";
  // docs/adr/0063: a mounted genome becomes an additional directory.
  const genome = mkdtempSync(join(tmpdir(), "genome-"));
  process.env.FLEET_GENOME_DIR = genome;
  try {
    const promise = runSession();
    await Bun.sleep(20);
    pushHuman("stop", "abort");
    await promise;
  } finally {
    delete process.env.FLEET_DEFINITION;
    delete process.env.FLEET_GENOME_DIR;
  }
  expect(queryOptions?.additionalDirectories).toEqual([genome]);
  const allowedTools = queryOptions?.allowedTools as string[];
  expect((queryOptions?.settings as { crossSessionInbound?: string })?.crossSessionInbound).toBe("refuse");
  expect((queryOptions?.settings as { attribution?: unknown })?.attribution).toEqual({ commit: "", pr: "", sessionUrl: false });
  expect(allowedTools).not.toContain("SendMessage");
  expect(allowedTools).not.toContain("ListAgents");
  expect(queryOptions?.disallowedTools).toEqual(["AskUserQuestion", "SendMessage", "ListAgents"]);
  expect(allowedTools).toContain("mcp__agent-fleet-sidecar__*");
});

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
  // Claude Code's native cross-session channel is open on purpose
  // (docs/adr/0060): usable without a prompt, and inbound delivered rather
  // than held — a held message has no one to approve it in a headless pod.
  expect(allowedTools).toContain("SendMessage");
  expect(allowedTools).toContain("ListAgents");
  expect(queryOptions?.disallowedTools).not.toContain("SendMessage");
  expect(queryOptions?.disallowedTools).not.toContain("ListAgents");
  expect((queryOptions?.settings as { crossSessionInbound?: string })?.crossSessionInbound).toBe("accept");
  expect((queryOptions?.settings as { remoteControlAtStartup?: boolean })?.remoteControlAtStartup).toBe(false);
  expect((queryOptions?.settings as { attribution?: unknown })?.attribution).toEqual({ commit: FLEET_TRAILER, pr: "", sessionUrl: false });
  // docs/adr/0063: no /genome in the test environment, so no packs directory.
  expect(queryOptions?.additionalDirectories).toEqual([]);
  expect((queryOptions?.env as Record<string, string>)?.CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD).toBe("1");
  const env = queryOptions?.env as Record<string, string>;
  const n = Number(env.GIT_CONFIG_COUNT) - 1;
  expect(env[`GIT_CONFIG_KEY_${n}`]).toBe("core.hooksPath");
  // The sidecar's ceiling is ours, and must stay above the transport's 60s
  // or it hard-fails every sidecar tool (docs/adr/0058).
  const sidecar = (queryOptions?.mcpServers as Record<string, { timeout?: number }>)["agent-fleet-sidecar"];
  expect(sidecar.timeout).toBe(SIDECAR_MCP_TIMEOUT_MS);
  expect(SIDECAR_MCP_TIMEOUT_MS).toBeGreaterThanOrEqual(60_000);
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

// The property issue #229 restores. canUseTool used to run the command
// through rtk between posting the permission_request and parking the
// resolver, so the request carried `go test ./...` while the resolver handed
// back `rtk go test ./...` — a human approved one command and a different one
// executed. Asserted on both resolve paths, since the rewrite reached both.
test("the command a human is shown is the command that executes", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  const command = "go test ./...";
  const call = capturedCanUseTool!("Bash", { command });
  await Bun.sleep(20);

  const req = findPermissionRequest("Bash");
  expect(JSON.parse(req!.text).input.command).toBe(command);

  respondToPermission(req!.seq, { behavior: "allow" });
  const result = await call;
  expect(result.behavior).toBe("allow");
  expect((result.updatedInput as { command: string }).command).toBe(command);

  pushHuman("", "abort");
  await promise;
}, 10000);

// The other resolve path: a mode switch to `auto` allows the oldest parked
// call (resolveOnePendingAllow, docs/adr/0052) without a human reading it
// again — so it must hand back the same input too.
test("a permission-mode switch allows the command as written, not a rewritten one", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  const command = "golangci-lint run";
  const call = capturedCanUseTool!("Bash", { command });
  await Bun.sleep(20);

  pushHuman("auto", "permission_mode");
  const result = await call;
  expect(result.behavior).toBe("allow");
  expect((result.updatedInput as { command: string }).command).toBe(command);

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

// After a resume the question may be gone from the conversation (k-k1's ADR
// 0055 D4/D5), so the delivered turn must carry it, plus the fixed line that
// stops Claude re-asking.
test("formatAnswerTurn carries each question, its answer, and the no-re-ask line", () => {
  const turn = formatAnswerTurn('{"answers":{"Which storage class?":"longhorn","Run migrations?":"yes"}}', 12);
  expect(turn).toContain("seq 12");
  expect(turn).toContain("Q: Which storage class?\nA: longhorn");
  expect(turn).toContain("Q: Run migrations?\nA: yes");
  expect(turn.endsWith(ANSWER_NO_REASK)).toBe(true);
});

test("formatAnswerTurn falls back to the raw text when the answer is not the dashboard's shape", () => {
  for (const raw of ["just go with postgres", "null", '{"answers":["a"]}', '{"answers":{"q":{"nested":1}}}', '{"answers":{}}']) {
    const turn = formatAnswerTurn(raw);
    expect(turn).toContain(raw);
    expect(turn).toContain("seq ?");
    expect(turn.endsWith(ANSWER_NO_REASK)).toBe(true);
  }
});

// The call site, not just the formatter: a delivered answer must reach the
// SDK as the formatted turn.
test("an answer entry reaches the SDK carrying the question text and the no-re-ask line", async () => {
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman('{"answers":{"Which storage class?":"longhorn"}}', "answer", 5);
  await Bun.sleep(20);
  const turn = consumedInputs.map((m) => m.message.content).find((t) => t.includes("Which storage class?"));
  expect(turn).toBeDefined();
  expect(turn).toContain("A: longhorn");
  expect(turn).toContain(ANSWER_NO_REASK);

  pushHuman("", "abort");
  await promise;
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

// docs/adr/0061. Sessions launch in auto by default now, and the SDK can refuse
// it at boot just as it refuses a mid-session switch. The gate must follow what
// the SDK reports, not what the column asked for.
test("auto REFUSED at launch: gate falls back to asking, column corrected", async () => {
  sessionPermissionMode = "auto";
  initPermissionMode = "default";
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start");
  await Bun.sleep(50);

  expect(savedPermissionModes).toContain("default");
  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull(); // asked a human, not auto-allowed
  expect(findPermissionRequest("Write")).toBeDefined();

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// The real SDK re-emits init every turn (seen live on 2.1.301). Only the first
// is the launch verdict; a later one must not override the mode the gate holds.
test("a later init does not override the launch mode", async () => {
  sessionPermissionMode = "default";
  extraMessages = [{ type: "system", subtype: "init", session_id: "x", permissionMode: "auto" }];
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start");
  await Bun.sleep(50);

  expect(savedPermissionModes).not.toContain("auto");
  // docs/adr/0063: once, at the first init, the pod reports what it loaded.
  const report = pushedMessages.find((m) => m.type === "system" && m.text.includes('"context_loaded"'));
  expect(report && JSON.parse(report.text)).toEqual({
    sdk: "context_loaded",
    memory_files: [{ path: "/genome/.claude/rules/90-fleet.md", type: "Project" }],
    skills: [
      { name: "journal", source: "projectSettings" },
      { name: "journal", source: "userSettings" },
    ],
    collisions: ["journal"],
  });
  expect(pushedMessages.filter((m) => m.text.includes('"context_loaded"'))).toHaveLength(1);
  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Write", { file_path: "x" }).then((r) => {
    resolved = r as { behavior: string };
  });
  await Bun.sleep(20);
  expect(resolved).toBeNull(); // still default: asked a human
  expect(findPermissionRequest("Write")).toBeDefined();

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

// `rm` at the head of the line is the case nobody writes. These are what an
// agent actually emits, and every one of them was allowed by the first version
// of this gate — one minute after a confirm reading "only rm and sudo still
// come to you".
test.each([
  "make build && sudo install",
  'bash -c "rm -rf /workspace/node_modules"',
  "sh -c 'rm -rf dist'",
  "/bin/rm -rf /workspace/x",
  'ssh host "sudo reboot"',
])("auto mode still asks for %p", async (command) => {
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: unknown = null;
  const call = capturedCanUseTool!("Bash", { command }).then((r) => (resolved = r));
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("Bash")).toBeDefined();

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// A plan is the one thing whose whole value is a human reading it, and
// AUTO_MODE_WARNING promises "and so does the next plan".
test("auto mode still asks for ExitPlanMode", async () => {
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: unknown = null;
  const call = capturedCanUseTool!("ExitPlanMode", { plan: "do the thing" }).then((r) => (resolved = r));
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("ExitPlanMode")).toBeDefined();

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// The mode switch pre-empts ONE parked call, and picking `auto` from the list
// is a click whose confirm says rm and sudo still come to you. Allowing a
// parked `rm` on that click would make the confirm a lie.
test("switching to auto does not allow a parked call auto itself would still ask about", async () => {
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: unknown = null;
  const call = capturedCanUseTool!("Bash", { command: "rm -rf /workspace/x" }).then((r) => (resolved = r));
  await Bun.sleep(20);

  pushHuman("auto", "permission_mode");
  await Bun.sleep(20);
  expect(setPermissionModeCalls).toContain("auto");
  expect(resolved).toBeNull(); // still a human's call

  pushHuman("", "abort");
  await promise;
  await call;
}, 10000);

// `plan` means "do not act yet", and the SDK's own plan-mode MCP gate is
// exactly what the FLEET_OWN_TOOL short-circuit overrides — so the sidecar
// tools that act OUTSIDE this session keep the gate there.
test("plan mode still asks for a sidecar tool that acts outside this session", async () => {
  sessionPermissionMode = "plan";
  const promise = runSession();
  await Bun.sleep(20);

  // Talking to a human is never a question, in any mode — that is the bug.
  expect((await capturedCanUseTool!("mcp__agent-fleet-sidecar__send_message", { text: "hi" })).behavior).toBe("allow");
  expect((await capturedCanUseTool!("mcp__agent-fleet-sidecar__journal_write", { text: "note" })).behavior).toBe("allow");

  let resolved: unknown = null;
  const call = capturedCanUseTool!("mcp__agent-fleet-sidecar__prompt_agent", { sessionId: "other" }).then(
    (r) => (resolved = r),
  );
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("mcp__agent-fleet-sidecar__prompt_agent")).toBeDefined();

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

// The transcript row above carries the whole `usage` object, so a missing
// field there is invisible; the LOG line is a hand-picked subset, which is
// exactly where one goes missing unnoticed. cacheCreationInputTokens did:
// writes bill at 1.25x base against reads at 0.1x, so it is the field that
// carries the cost, and without it on the line "where did the tokens go" had
// to be answered by solving for it as a residual of totalCostUsd against an
// assumed price table. Assert all four together — the subset is only useful
// if it stays complete.
test("the result log line carries all four token fields, not three", async () => {
  forceResult = {
    subtype: "success",
    num_turns: 3,
    total_cost_usd: 0.42,
    usage: {
      input_tokens: 100,
      output_tokens: 20,
      cache_read_input_tokens: 3000,
      cache_creation_input_tokens: 5000,
    },
    modelUsage: {
      "claude-opus-5": { inputTokens: 90, outputTokens: 15, cacheReadInputTokens: 2500, cacheCreationInputTokens: 4000 },
      "claude-haiku-4-5": { inputTokens: 10, outputTokens: 5, cacheReadInputTokens: 500, cacheCreationInputTokens: 1000 },
    },
  };

  const lines: string[] = [];
  const origLog = console.log;
  console.log = (s: string) => lines.push(s);
  try {
    await relayOnce([]);
  } finally {
    console.log = origLog;
  }

  const resultLine = lines
    .map((l) => JSON.parse(l) as Record<string, unknown>)
    .find((l) => typeof l.msg === "string" && (l.msg as string).endsWith("result"));

  expect(resultLine).toBeDefined();
  expect(resultLine).toMatchObject({
    inputTokens: 100,
    outputTokens: 20,
    cacheReadInputTokens: 3000,
    cacheCreationInputTokens: 5000,
    // and the modelUsage roll-up alongside it — the top-level usage above is
    // only the main model, so these are the numbers that describe the run.
    models: ["claude-haiku-4-5", "claude-opus-5"],
    cacheReadInputTokensAll: 3000,
    cacheCreationInputTokensAll: 5000,
  });
});

test("sumModelUsage totals every model, and returns null rather than a fake zero", () => {
  expect(
    sumModelUsage({
      b: { inputTokens: 1, outputTokens: 2, cacheReadInputTokens: 3, cacheCreationInputTokens: 4 },
      a: { inputTokens: 10, outputTokens: 20, cacheReadInputTokens: 30, cacheCreationInputTokens: 40 },
    }),
  ).toEqual({
    models: ["a", "b"],
    inputTokensAll: 11,
    outputTokensAll: 22,
    cacheReadInputTokensAll: 33,
    cacheCreationInputTokensAll: 44,
  });

  // A model entry missing a field contributes 0 for it, not NaN — one absent
  // key must not blank the whole roll-up.
  expect(sumModelUsage({ a: { outputTokens: 5 } })).toMatchObject({
    inputTokensAll: 0,
    outputTokensAll: 5,
    cacheReadInputTokensAll: 0,
  });

  // Absent/!object/empty => null, so the log line omits the fields entirely.
  // A 0 here would read in LogQL as "this run used no tokens", which is a
  // different and wrong claim from "the SDK told us nothing".
  for (const bad of [undefined, null, {}, [], "nope", 7]) {
    expect(sumModelUsage(bad)).toBeNull();
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

// `cat` is the canonical way to print a file, so an agent reaching for the
// worker's own environment reaches for `cat /proc/self/environ` — which puts
// GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN into a transcript the dashboard
// renders. It was allow-listed for a while against fleet-shared/README.md's
// own stated policy; this pins the two together so the next edit has to
// disagree with a failing test rather than only with prose.
//
// Deliberately NOT extended to head/nl/grep/sort/cut/etc: they read the same
// file just as well, and a Bash allow-rule matches a command prefix rather
// than a path, so covering them all would mean deleting read-only Bash. This
// reduces the accidental path; it is not a boundary. See the README.
test("fleet-shared/settings.json does not allow-list cat", async () => {
  const settings = await Bun.file(new URL("../../fleet-shared/settings.json", import.meta.url)).json();
  expect(settings.permissions.allow).not.toContain("Bash(cat:*)");
});

// --- docs/adr/0067 §4: a definition session applies its AgentSpec ---

const DEF_SPEC = {
  toolsAllow: ["Read", "Edit", "Bash(git add:*)", "Bash(git status)"],
  toolsDeny: ["Edit(.git/**)"],
  sidecarTools: ["AskUserQuestion", "send_message"],
  modelAllow: ["claude-opus-5"],
  thinkingDefault: "medium",
  modeAllow: ["auto", "default", "plan"],
  systemPromptAppend: "Keep pages small.",
  maxTurns: 60,
  interaction: "",
  lifecycle: "",
};

test.each([
  [["Read"], "Read", {}, "Read"],
  [["Read"], "Write", {}, undefined],
  [["Bash(git add:*)"], "Bash", { command: "git add ." }, "Bash(git add:*)"],
  [["Bash(git add:*)"], "Bash", { command: "git add" }, "Bash(git add:*)"],
  [["Bash(git add:*)"], "Bash", { command: "git addx" }, undefined],
  [["Bash(git add:*)"], "Bash", { command: " git add ." }, undefined],
  [["Bash(git add:*)"], "Read", { command: "git add ." }, undefined],
  [["Bash(git status)"], "Bash", { command: "git status" }, "Bash(git status)"],
  [["Bash(git status)"], "Bash", { command: "git status; rm -rf /" }, undefined],
  // A non-asking rule wins over a prefix rule on the same call.
  [["Bash(git:*)", "Bash(git status)"], "Bash", { command: "git status" }, "Bash(git status)"],
  // Only the forms the definitions use: anything else matches nothing.
  [["Edit(src/**)"], "Edit", { file_path: "src/a.ts" }, undefined],
  [["Bash"], "Bash", { command: "curl x" }, "Bash"],
] as const)("matchesToolRule(%j, %s, %j) = %s", (rules, tool, input, want) => {
  expect(matchesToolRule([...rules], tool, input as Record<string, unknown>)).toBe(want);
});

// A real worktree on disk, so symlinks resolve: .git and .claude inside it, and
// `out` a symlink to a directory outside it.
const WT = mkdtempSync(join(tmpdir(), "wt-"));
mkdirSync(join(WT, ".git"));
mkdirSync(join(WT, ".claude"));
mkdirSync(join(WT, "notes"));
mkdirSync(join(WT, "sub", ".git", "hooks"), { recursive: true });
const OUTSIDE = mkdtempSync(join(tmpdir(), "outside-"));
symlinkSync(OUTSIDE, join(WT, "out"));
symlinkSync(join(OUTSIDE, "missing"), join(WT, "dangling"));

const DEF_ALLOW = { ...DEF_SPEC, toolsAllow: ["Read", "Edit", "Write", "Bash(git add:*)", "Bash(git push:*)", "Bash(git status)", "Bash(rm:*)", "Bash(npm test:*)"] };

test.each([
  // docs/adr/0076 §2 step 2: inside the worktree only, whatever tools.allow says.
  ["auto", "Edit", { file_path: "notes/a.md" }, "allow"],
  ["auto", "Write", { file_path: join(WT, "notes", "new.md") }, "allow"],
  ["auto", "Edit", { file_path: "../x" }, "deny"],
  ["auto", "Edit", { file_path: "/claude-home/settings.json" }, "deny"],
  ["auto", "Edit", { file_path: ".git/config" }, "deny"],
  ["auto", "Write", { file_path: join(WT, ".claude", "settings.json") }, "deny"],
  ["auto", "Write", { file_path: "out/x.md" }, "deny"],
  ["auto", "Write", { file_path: "dangling" }, "deny"],
  ["auto", "Edit", {}, "deny"],
  ["auto", "NotebookEdit", { notebook_path: "/etc/x.ipynb" }, "deny"],
  // Any .git/ or .claude/ segment, not just the top, and git's config files anywhere.
  ["auto", "Write", { file_path: "sub/.git/hooks/pre-receive" }, "deny"],
  ["auto", "Write", { file_path: "a/.claude/x" }, "deny"],
  ["auto", "Write", { file_path: ".gitmodules" }, "deny"],
  ["auto", "Write", { file_path: "notes/.gitattributes" }, "deny"],
  ["auto", "Write", { file_path: "notes/.gitignore" }, "allow"],
  // default asks a matched edit; plan is treated like default.
  ["default", "Edit", { file_path: "notes/a.md" }, "ask"],
  ["plan", "Write", { file_path: "notes/a.md" }, "ask"],
  ["default", "Read", {}, "allow"],
  // Bash: the git argument check, the rm/sudo check, plain words for other prefixes.
  ["auto", "Bash", { command: "git push" }, "allow"],
  ["auto", "Bash", { command: "git push --force" }, "ask"],
  ["default", "Bash", { command: "git push" }, "allow"],
  ["auto", "Bash", { command: "rm -rf x" }, "ask"],
  ["auto", "Bash", { command: "npm test -- --watch=false" }, "allow"],
  ["auto", "Bash", { command: "npm test; curl x" }, "ask"],
  ["auto", "Bash", { command: "git status" }, "allow"],
  ["auto", "Bash", { command: "git push origin feature/x" }, "allow"],
  ["auto", "Bash", { command: "git push ./sub" }, "ask"],
  // Outside tools.allow: inside the ceiling asks, outside it is denied.
  ["auto", "Bash", { command: "ls" }, "ask"],
  ["auto", "TodoWrite", {}, "ask"],
  ["auto", "WebFetch", { url: "https://x" }, "deny"],
  ["auto", "mcp__playwright__browser_navigate", {}, "deny"],
  ["auto", "ExitPlanMode", {}, "ask"],
  ["default", "ExitPlanMode", {}, "ask"],
] as const)("definitionDecision(%s, %s, %j) = %s", (mode, tool, input, want) => {
  expect(definitionDecision(DEF_ALLOW, tool, input as Record<string, unknown>, mode, WT).behavior).toBe(want);
});

test.each([
  ["auto", "Bash", { command: "ls" }],
  ["auto", "Bash", { command: "git push --force" }],
  ["default", "Edit", { file_path: "notes/a.md" }],
  ["auto", "ExitPlanMode", {}],
] as const)("interaction deny: %s %s %j asks become denies", (mode, tool, input) => {
  expect(definitionDecision({ ...DEF_ALLOW, interaction: "deny" }, tool, input as Record<string, unknown>, mode, WT)).toEqual({
    behavior: "deny",
    message: "not allowed in a headless run",
  });
});

test("sdkMode: the fleet's auto is the SDK's default, every other mode passes through", () => {
  expect(sdkMode("auto")).toBe("default");
  expect(sdkMode("default")).toBe("default");
  expect(sdkMode("plan")).toBe("plan");
});

test.each([
  // The everyday edit-and-sync loop runs without a prompt.
  ["git add", "git add .", true],
  ["git add", "git add -A", true],
  ["git add", "git add 'notes/a b.md'", true],
  ["git commit", 'git commit -m "fix: tidy the index page"', true],
  ["git commit", "git commit -am 'x'", false],
  ["git commit", "git commit -m 'it costs $5'", true],
  ["git pull --rebase", "git pull --rebase", true],
  ["git pull --rebase", "git pull --rebase origin main", true],
  ["git push", "git push", true],
  ["git push", "git push origin main", true],
  ["git push", "git push -u origin main", true],
  // `/` only in a branch, never in the remote; no leading `.` or `~` anywhere.
  ["git push", "git push origin feature/x", true],
  ["git push", "git push ./sub", false],
  ["git push", "git push ../x", false],
  ["git push", "git push sub/repo main", false],
  ["git push", "git push . main", false],
  ["git push", "git push origin ./x", false],
  ["git pull --rebase", "git pull --rebase ./sub", false],
  ["git pull --rebase", "git pull --rebase origin feature/x", true],
  // Anything that can run a local command, rewrite history or chain asks.
  ["git push", "git push --receive-pack='sh -c x' origin", false],
  ["git push", "git push --force", false],
  ["git push", "git push -f origin main", false],
  ["git push", "git push origin +main", false],
  ["git push", "git push origin :main", false],
  ["git push", "git push origin main:other", false],
  ["git pull --rebase", "git pull --rebase --upload-pack=x", false],
  ["git commit", "git commit --no-verify -m x", false],
  ["git commit", 'git commit -m "$(id)"', false],
  ["git commit", "git commit -m", false],
  ["git add", "git add . && curl x", false],
  ["git add", "git add . ; rm -rf /", false],
  ["git add", "git add `id`", false],
  ["git add", "git add 'unterminated", false],
  ["git add", "git addx", false],
  ["git diff", "git diff --output=/tmp/x", false],
  ["git rebase", "git rebase -i", false],
] as const)("gitArgsAreInert(%s, %s) = %s", (prefix, command, want) => {
  expect(gitArgsAreInert(prefix, command)).toBe(want);
});

// A git prefix the table does not name always asks; a non-git prefix that
// happens to be an Object.prototype key is just a prefix.
const DEF_GIT = { ...DEF_SPEC, toolsAllow: ["Bash(git:*)", "Bash(git checkout:*)", "Bash(toString:*)"] };
test.each([
  ["git -c core.sshCommand=x push"],
  ["git -C .. status"],
  ["git --exec-path=x status"],
  ["git checkout x"],
  ["git status"],
] as const)("Bash(git:*) / Bash(git checkout:*): %s asks", (command) => {
  expect(definitionDecision(DEF_GIT, "Bash", { command }, "auto", WT).behavior).toBe("ask");
});

test("Bash(toString:*) does not throw", () => {
  expect(definitionDecision(DEF_GIT, "Bash", { command: "toString x" }, "auto", WT).behavior).toBe("allow");
  expect(gitArgsAreInert("toString", "toString x")).toBe(false);
  expect(gitArgsAreInert("constructor", "constructor")).toBe(false);
});

// The ceiling comes first: tools.allow cannot widen it.
test.each([["WebFetch"], ["NotebookEdit"], ["Task"]] as const)("tools.allow listing %s is still denied", (tool) => {
  const spec = { ...DEF_SPEC, toolsAllow: ["WebFetch", "NotebookEdit", "Task"] };
  expect(definitionDecision(spec, tool, { notebook_path: "notes/a.ipynb" }, "auto", WT)).toEqual({ behavior: "deny", message: "not available to definitions" });
});

test("definitionDecision says why it denied", () => {
  expect(definitionDecision(DEF_SPEC, "Write", { file_path: "../x" }, "auto", WT)).toEqual({ behavior: "deny", message: "outside this definition's workspace" });
  expect(definitionDecision(DEF_SPEC, "WebFetch", {}, "auto", WT)).toEqual({ behavior: "deny", message: "not available to definitions" });
});

test("definition session: query() options come from the spec, and the stream carries its sha", async () => {
  sessionAgentSpec = DEF_SPEC;
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions?.settingSources).toEqual(["user"]);
  expect(queryOptions?.allowedTools).toEqual(["mcp__agent-fleet-sidecar__AskUserQuestion", "mcp__agent-fleet-sidecar__send_message"]);
  expect(queryOptions?.additionalDirectories).toEqual(["/workspace"]);
  expect((queryOptions?.env as Record<string, string>).CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD).toBe("1");
  const perms = (queryOptions?.settings as { permissions: { ask: string[]; deny?: string[] } }).permissions;
  expect(perms.deny).toEqual(["Edit(.git/**)"]);
  expect(perms.ask).toContain("Bash(gh:*)");
  expect(queryOptions?.effort).toBe("medium");
  expect(queryOptions?.thinking).toEqual({ type: "adaptive" });
  expect(queryOptions?.systemPrompt).toBe("Keep pages small.");
  expect(queryOptions?.maxTurns).toBe(60);
  expect(queryOptions?.permissionMode).toBe("default");
  expect(queryOptions?.tools).toEqual(["Read", "Edit", "Write", "Glob", "Grep", "Bash", "TodoWrite", "ExitPlanMode"]);
  expect(streamedSpecSha).toBe("spec-sha");
}, 10000);

// docs/adr/0076 §1: the SDK never runs in its own auto for a definition, and
// the session is not saved back as default when the SDK reports it.
test("definition session in auto: the SDK gets default, the session stays auto, a live auto switch sends default", async () => {
  sessionAgentSpec = DEF_SPEC;
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);
  expect(queryOptions?.permissionMode).toBe("default");
  expect(savedPermissionModes).not.toContain("default");

  pushHuman("plan", "permission_mode");
  await Bun.sleep(20);
  pushHuman("auto", "permission_mode");
  await Bun.sleep(20);
  expect(setPermissionModeCalls).toEqual(["plan", "default"]);
  // Still the fleet's auto: a matched edit runs without asking.
  expect((await capturedCanUseTool!("Edit", { file_path: "a.md" })).behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("repo session in auto: the SDK gets auto, and a live auto switch sends auto", async () => {
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);
  expect(queryOptions?.permissionMode).toBe("auto");

  pushHuman("default", "permission_mode");
  await Bun.sleep(20);
  pushHuman("auto", "permission_mode");
  await Bun.sleep(20);
  expect(setPermissionModeCalls).toEqual(["default", "auto"]);

  pushHuman("", "abort");
  await promise;
}, 10000);

test("repo session: no spec, so nothing of it reaches query() and the stream carries no sha", async () => {
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions?.settingSources).toEqual(["user", "project"]);
  expect((queryOptions?.settings as { permissions: { deny?: string[] } }).permissions.deny).toBeUndefined();
  expect(queryOptions?.effort).toBeUndefined();
  expect(queryOptions?.thinking).toBeUndefined();
  expect(queryOptions?.systemPrompt).toBeUndefined();
  expect(queryOptions && "tools" in queryOptions).toBe(false);
  expect(streamedSpecSha).toBe("");
}, 10000);

// docs/adr/0077: nothing chosen, nothing passed — Claude Code picks both.
test("repo session with no model or effort: query() gets neither", async () => {
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions && "model" in queryOptions).toBe(false);
  expect(queryOptions && "effort" in queryOptions).toBe(false);
  expect(queryOptions && "thinking" in queryOptions).toBe(false);
}, 10000);

// docs/adr/0077: the dialog's picks reach query(), and the row records the
// model init reported, not the one asked for.
test("repo session with a model and effort: both reach query(), init's model is saved", async () => {
  sessionModel = "claude-sonnet-5";
  sessionEffort = "high";
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("hello");
  await Bun.sleep(50);
  pushHuman("stop", "abort");
  await promise;

  expect(queryOptions?.model).toBe("claude-sonnet-5");
  expect(queryOptions?.effort).toBe("high");
  expect(queryOptions?.thinking).toEqual({ type: "adaptive" });
  expect(savedModels).toEqual(["claude-opus-4-8"]);
}, 10000);

// A hook message carries session_id and can precede init; the row still gets
// init's model, and /clear reports it too.
test("a hook message before init: init's model is still saved", async () => {
  preInitMessages = [{ type: "system", subtype: "hook_response", hook_name: "SessionStart:startup", hook_event: "SessionStart", output: "", stdout: "", stderr: "", outcome: "success" }];
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("hello");
  await Bun.sleep(50);
  pushHuman("/clear");
  await promise;

  // The hook's session-id save leaves the model alone, init saves it, /clear reuses it.
  expect(savedModels).toEqual(["", "claude-opus-4-8", "claude-opus-4-8"]);
  expect(savedSessionIds.at(-1)).toBe("");
}, 10000);

test("definition session in auto: canUseTool allows inside tools.allow, denies outside the ceiling, asks the rest", async () => {
  sessionAgentSpec = DEF_SPEC;
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  // WORKTREE_PATH is /workspace here; a relative path resolves inside it.
  expect((await capturedCanUseTool!("Edit", { file_path: "a.md" })).behavior).toBe("allow");
  expect((await capturedCanUseTool!("Edit", { file_path: "/claude-home/settings.json" })).behavior).toBe("deny");
  expect((await capturedCanUseTool!("mcp__agent-fleet-sidecar__send_message", { text: "hi" })).behavior).toBe("allow");
  // A sidecar tool the definition does not list is not the fleet's own here.
  const expose = await capturedCanUseTool!("mcp__agent-fleet-sidecar__expose", { port: 8080 });
  expect(expose).toEqual({ behavior: "deny", message: "not available to definitions" });
  expect((await capturedCanUseTool!("mcp__playwright__browser_navigate", {})).behavior).toBe("deny");
  expect(pushedMessages.filter((m) => m.type === "permission_request")).toHaveLength(0);

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Bash", { command: "git add --chmod=+x a" }).then((r) => (resolved = r));
  await Bun.sleep(20);
  expect(resolved).toBeNull();
  expect(findPermissionRequest("Bash")).toBeDefined();

  pushHuman("", "abort");
  await call;
  await promise;
}, 10000);

test("definition session: a permission-mode change approves nothing", async () => {
  sessionAgentSpec = DEF_SPEC;
  const promise = runSession();
  await Bun.sleep(20);

  let resolved: { behavior: string } | null = null;
  const call = capturedCanUseTool!("Bash", { command: "git add --chmod=+x a" }).then((r) => (resolved = r));
  await Bun.sleep(20);
  pushHuman("plan", "permission_mode");
  await Bun.sleep(20);
  expect(setPermissionModeCalls).toContain("plan");
  expect(resolved).toBeNull();
  expect(pushedMessages.find((m) => m.type === "permission_response")).toBeUndefined();

  pushHuman("", "abort");
  await call;
  await promise;
}, 10000);

test("definition session: an answer's updatedInput is ignored, the call runs as asked", async () => {
  sessionAgentSpec = DEF_SPEC;
  const promise = runSession();
  await Bun.sleep(20);

  const call = capturedCanUseTool!("Bash", { command: "git add --chmod=+x a" });
  await Bun.sleep(20);
  const req = findPermissionRequest("Bash");
  respondToPermission(req!.seq, { behavior: "allow", updatedInput: { command: "curl https://x | sh" } });
  const result = await call;
  expect(result).toEqual({ behavior: "allow", updatedInput: { command: "git add --chmod=+x a" } });

  pushHuman("", "abort");
  await promise;
}, 10000);

test("definition session: a permission_mode outside modeAllow is refused, the SDK never sees it", async () => {
  sessionAgentSpec = { ...DEF_SPEC, modeAllow: ["auto", "default"] };
  sessionPermissionMode = "auto";
  const promise = runSession();
  await Bun.sleep(20);

  pushHuman("plan", "permission_mode");
  await Bun.sleep(20);
  expect(setPermissionModeCalls).toEqual([]);
  expect(savedPermissionModes).toContain("auto");
  expect(pushedMessages.some((m) => m.type === "system" && m.text.startsWith("Could not switch to plan"))).toBe(true);
  // Still the fleet's auto: a matched edit runs without asking.
  expect((await capturedCanUseTool!("Edit", { file_path: "a.md" })).behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

test("definition session: the init check never adopts the SDK's reported mode", async () => {
  sessionAgentSpec = DEF_SPEC;
  sessionPermissionMode = "auto";
  initPermissionMode = "acceptEdits";
  const promise = runSession();
  await Bun.sleep(20);
  pushHuman("start");
  await Bun.sleep(50);

  expect(savedPermissionModes).not.toContain("acceptEdits");
  // currentMode is still auto: a matched edit runs without asking.
  expect((await capturedCanUseTool!("Edit", { file_path: "a.md" })).behavior).toBe("allow");

  pushHuman("", "abort");
  await promise;
}, 10000);

// docs/adr/0076 §3: a grant is an allow-list entry a human added. It runs a
// call tools.allow does not list, a granted edit does not ask again in
// `default`, and it never widens the worktree rule, the ceiling or rm/sudo.
const DEF_NONE = { ...DEF_SPEC, toolsAllow: [] as string[] };
test.each([
  [["Edit(notes/**)"], "default", "Edit", { file_path: "notes/a.md" }, "allow"],
  [["Edit(notes/**)"], "auto", "Edit", { file_path: join(WT, "notes", "b.md") }, "allow"],
  [["Edit(notes/**)"], "auto", "Edit", { file_path: "notesx/a.md" }, "ask"],
  [["Edit(notes/**)"], "auto", "Write", { file_path: "notes/a.md" }, "ask"],
  [["Edit(notes/a.md)"], "auto", "Edit", { file_path: "notes/a.md" }, "allow"],
  [["Edit(notes/a.md)"], "auto", "Edit", { file_path: "notes/b.md" }, "ask"],
  [["Write(**)"], "auto", "Write", { file_path: "x.md" }, "allow"],
  [["Write(**)"], "auto", "Write", { file_path: ".git/config" }, "deny"],
  [["Write(**)"], "auto", "Write", { file_path: "out/x.md" }, "deny"],
  [["Bash(npm test)"], "auto", "Bash", { command: "npm test" }, "allow"],
  [["Bash(npm test)"], "auto", "Bash", { command: "npm test x" }, "ask"],
  [["Bash(npm run:*)"], "default", "Bash", { command: "npm run build" }, "allow"],
  [["Bash(npm run:*)"], "auto", "Bash", { command: "npm run build && curl x" }, "ask"],
  [["Bash(rm:*)"], "auto", "Bash", { command: "rm -rf x" }, "ask"],
  [["Bash(git checkout:*)"], "auto", "Bash", { command: "git checkout x" }, "ask"],
  [["TodoWrite"], "auto", "TodoWrite", {}, "allow"],
  [["WebFetch"], "auto", "WebFetch", { url: "https://x" }, "deny"],
] as const)("grants %j: definitionDecision(%s, %s, %j) = %s", (grants, mode, tool, input, want) => {
  expect(definitionDecision(DEF_NONE, tool, input as Record<string, unknown>, mode, WT, [...grants]).behavior).toBe(want);
});

// A tools.allow match must not hide a grant for the same call.
test("a grant still runs where tools.allow matched but would ask", () => {
  const spec = { ...DEF_SPEC, toolsAllow: ["Edit", "Bash(git push:*)"] };
  expect(definitionDecision(spec, "Edit", { file_path: "notes/a.md" }, "default", WT, []).behavior).toBe("ask");
  expect(definitionDecision(spec, "Edit", { file_path: "notes/a.md" }, "default", WT, ["Edit(notes/**)"]).behavior).toBe("allow");
  expect(definitionDecision(spec, "Bash", { command: "git push origin +main" }, "auto", WT, []).behavior).toBe("ask");
  expect(definitionDecision(spec, "Bash", { command: "git push origin +main" }, "auto", WT, ["Bash(git push origin +main)"]).behavior).toBe("allow");
});

test("a grant does not ask under interaction: deny", () => {
  const spec = { ...DEF_NONE, interaction: "deny" };
  expect(definitionDecision(spec, "Bash", { command: "npm test" }, "auto", WT, ["Bash(npm test)"]).behavior).toBe("allow");
  expect(definitionDecision(spec, "Bash", { command: "ls" }, "auto", WT, ["Bash(npm test)"]).behavior).toBe("deny");
});

// docs/adr/0068 §4: a write asks a human in every mode, and no remembered
// grant covers it, when the file is not a plain type or will hold any active
// content.
describe("script-write ask", () => {
  const page = join(WT, "page.md");
  const scripted = join(WT, "scripted.md");
  writeFileSync(page, "Price {total} today.\n```lua\nprint(1)\n```\n");
  writeFileSync(scripted, "Intro\n```space-lua\nlocal x = 1\n```\nTotal ${x}\n");
  const ISLAND = { ...DEF_ALLOW, scriptAsk: true };
  const decide = (tool: string, input: Record<string, unknown>, grants: string[] = []) =>
    definitionDecision(ISLAND, tool, input, "auto", WT, grants).behavior;

  test("the script ask is marked so core offers no remembered rule", () => {
    expect(definitionDecision(ISLAND, "Write", { file_path: "new.md", content: "${x}" }, "auto", WT)).toEqual({ behavior: "ask", noGrant: true });
    expect(definitionDecision(ISLAND, "Bash", { command: "ls" }, "auto", WT)).toEqual({ behavior: "ask" });
  });

  test("off for a definition no island host serves (a code repo)", () => {
    expect(definitionDecision(DEF_ALLOW, "Write", { file_path: "src/a.ts", content: "`${x}`" }, "auto", WT).behavior).toBe("allow");
  });

  test.each([
    ["a space-lua fence", { file_path: "new.md", content: "```space-lua\nprint(1)\n```" }],
    ["a tilde fence", { file_path: "new.md", content: "~~~space-lua\nprint(1)\n~~~" }],
    ["a case variant", { file_path: "new.md", content: "``` Space-Script\nx\n```" }],
    ["a style fence", { file_path: "new.md", content: "```space-style\n.a{}\n```" }],
    ["an expression", { file_path: "new.md", content: "Total: ${query[[from index.tag 'x']]}" }],
    ["js.import", { file_path: "new.md", content: "local m = js.import('https://x')" }],
    ["raw HTML script", { file_path: "new.md", content: "<img src=x onerror=alert(1)>" }],
    ["a script tag", { file_path: "new.md", content: "<script>alert(1)</script>" }],
    ["a javascript: link", { file_path: "new.md", content: "[x](javascript:alert(1))" }],
    ["a .js file", { file_path: "lib/x.js", content: "1" }],
    ["an .html file", { file_path: "x.html", content: "hi" }],
    ["an .svg file", { file_path: "x.svg", content: "<svg/>" }],
    ["a file with no extension", { file_path: "notes", content: "plain" }],
    ["a library install", { file_path: "Library/Std/x.md", content: "plain" }],
  ])("Write with %s asks", (_, input) => {
    expect(decide("Write", input)).toBe("ask");
    expect(decide("Write", input, ["Write"])).toBe("ask");
  });

  test("any edit to a page holding a script asks, even one that adds no marker", () => {
    expect(decide("Edit", { file_path: "scripted.md", old_string: "local x = 1", new_string: "local x = os.exit()" })).toBe("ask");
    expect(decide("Edit", { file_path: "scripted.md", old_string: "Intro", new_string: "Hello" })).toBe("ask");
  });

  test("an Edit that joins a marker from pieces asks", () => {
    expect(decide("Edit", { file_path: "page.md", old_string: "Price ", new_string: "Price $" })).toBe("ask");
    expect(decide("Edit", { file_path: "page.md", old_string: "```lua", new_string: "```space-lua" })).toBe("ask");
  });

  test("$-patterns in new text are literal", () => {
    // With a string replacer, "$&" would re-insert "Price " and hide nothing,
    // but it must be judged as the literal text the SDK writes.
    expect(decide("Edit", { file_path: "page.md", old_string: "today", new_string: "$& more" })).toBe("allow");
  });

  test("plain writes and edits still run", () => {
    expect(decide("Write", { file_path: "new.md", content: "# Notes\nNothing to run, only {braces} and $5." })).toBe("allow");
    expect(decide("Write", { file_path: "img/cat.png", content: "x" })).toBe("allow");
    expect(decide("Edit", { file_path: "page.md", old_string: "today", new_string: "tomorrow" })).toBe("allow");
  });
});
