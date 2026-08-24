import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";
import {
  latestSlashCommands,
  latestSystemInfo,
  parseSdkSignal,
  parseSdkSystemInfo,
  permissionDenyMessages,
  buildToolCallPairs,
  feedVisibility,
  listSummary,
  inFlightTool,
  hasPendingDecision,
  entryTime,
  resultDelta,
  withOptimistic,
  findPendingPermissions,
  resolvedPermissionDecisions,
  subagentRuns,
  fileEdits,
  decisionAnswerable,
  backgroundTasks,
} from "./transcript";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, from = "agent", text = ""): TranscriptEntry {
  nextSeq += 1n;
  return { $typeName: "agentfleet.v1.TranscriptEntry", sessionId: "t", seq: nextSeq, from, text, type };
}

// A SYSTEM entry is now two different things, and telling them apart is
// load-bearing: the worker relays every out-of-band SDK signal under this
// same entry type, so anything that assumed "SYSTEM means session init"
// silently started reading a tool_progress instead.
const INIT_TEXT = JSON.stringify({ model: "opus", slashCommands: ["compact"], mcpServers: [{ name: "sidecar", status: "connected" }] });
const PROGRESS_TEXT = JSON.stringify({ sdk: "tool_progress", tool_name: "Bash", elapsed_time_seconds: 32 });

test("parseSdkSystemInfo refuses a signal entry, parseSdkSignal refuses an init entry", () => {
  expect(parseSdkSystemInfo(PROGRESS_TEXT)).toBeNull();
  expect(parseSdkSignal(INIT_TEXT)).toBeNull();
  expect(parseSdkSystemInfo(INIT_TEXT)?.model).toBe("opus");
  expect(parseSdkSignal(PROGRESS_TEXT)?.sdk).toBe("tool_progress");
});

test("slash commands survive signal entries arriving after session init", () => {
  const entries = [
    entry(TranscriptEntryType.SYSTEM, "agent", INIT_TEXT),
    entry(TranscriptEntryType.SYSTEM, "agent", PROGRESS_TEXT),
  ];
  // Stopping at the newest SYSTEM entry of any kind emptied the command
  // palette the moment any signal followed init.
  expect(latestSlashCommands(entries)).toEqual(["compact"]);
  expect(latestSystemInfo(entries)?.mcpServers?.[0].status).toBe("connected");
});

test("a denial's reason is recovered, an allow contributes nothing", () => {
  const request = entry(TranscriptEntryType.PERMISSION_REQUEST);
  const denied = entry(TranscriptEntryType.PERMISSION_RESPONSE, "human", JSON.stringify({ behavior: "deny", message: "wrong file" }));
  denied.replyTo = request.seq;
  const allowRequest = entry(TranscriptEntryType.PERMISSION_REQUEST);
  const allowed = entry(TranscriptEntryType.PERMISSION_RESPONSE, "human", JSON.stringify({ behavior: "allow" }));
  allowed.replyTo = allowRequest.seq;

  const messages = permissionDenyMessages([request, denied, allowRequest, allowed]);
  expect(messages.get(request.seq)).toBe("wrong file");
  expect(messages.has(allowRequest.seq)).toBe(false);
});

test("tool calls pair with their results, and thinking blocks are not calls", () => {
  const call = entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_1", tool: "Bash", input: { command: "ls" } }));
  const thinking = entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ kind: "thinking", text: "hmm" }));
  const result = entry(TranscriptEntryType.USER, "agent", JSON.stringify({ toolUseId: "toolu_1", isError: false, content: "a.ts" }));

  const pairs = buildToolCallPairs([call, thinking, result]);
  expect(pairs.length).toBe(1);
  expect(pairs[0].callInfo.tool).toBe("Bash");
  expect(pairs[0].resultInfo?.content).toBe("a.ts");
});

test("a call still awaiting its result pairs with null, not with someone else's result", () => {
  const inFlight = entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_pending", tool: "Bash", input: {} }));
  const otherResult = entry(TranscriptEntryType.USER, "agent", JSON.stringify({ toolUseId: "toolu_other", content: "x" }));

  const pairs = buildToolCallPairs([inFlight, otherResult]);
  expect(pairs[0].result).toBeNull();
});

// --- console rewrite derivations ---------------------------------------------

// The shared fixture above omits created_at/reply_to; these derivations read
// both, so they get their own builder rather than loosening that one.
function full(
  type: TranscriptEntryType,
  text: string,
  opts: { from?: string; replyTo?: bigint; createdAt?: string } = {},
): TranscriptEntry {
  nextSeq += 1n;
  return {
    $typeName: "agentfleet.v1.TranscriptEntry",
    sessionId: "t",
    seq: nextSeq,
    from: opts.from ?? "agent",
    text,
    type,
    replyTo: opts.replyTo,
    createdAt: opts.createdAt ?? "",
  } as TranscriptEntry;
}

const perm = (tool: string, input: unknown = {}) =>
  full(TranscriptEntryType.PERMISSION_REQUEST, JSON.stringify({ tool, input }));
const respond = (replyTo: bigint, behavior: "allow" | "deny", message?: string) =>
  full(TranscriptEntryType.PERMISSION_RESPONSE, JSON.stringify({ behavior, message }), { from: "human", replyTo });
const toolUse = (id: string, tool: string, input: unknown = {}) =>
  full(TranscriptEntryType.ASSISTANT, JSON.stringify({ id, tool, input }));
const toolResult = (toolUseId: string) =>
  full(TranscriptEntryType.USER, JSON.stringify({ toolUseId, content: "ok" }));

// The two mockups' renderVals() disagree on the third mode on purpose: mobile's
// "calls" shows tool activity, desktop's "decisions" shows nothing but the
// tier-1 cards. Encoding one and reusing it for both is the bug this guards.
test("feedVisibility: mobile 'decisions' keeps tools, desktop 'decisions' does not", () => {
  expect(feedVisibility("decisions", false)).toEqual({ narrative: false, tools: false, quiet: false });
  expect(feedVisibility("decisions", true)).toEqual({ narrative: false, tools: true, quiet: false });
  // The other two modes agree across form factors.
  for (const isMobile of [false, true]) {
    expect(feedVisibility("everything", isMobile)).toEqual({ narrative: true, tools: true, quiet: true });
    expect(feedVisibility("narrative", isMobile)).toEqual({ narrative: true, tools: false, quiet: false });
  }
});

test("listSummary surfaces the pending permission, question and in-flight tool", () => {
  const entries = [
    full(TranscriptEntryType.ASSISTANT, JSON.stringify({ id: "t1", tool: "TodoWrite", input: { todos: [
      { content: "a", status: "completed", activeForm: "a" },
      { content: "b", status: "in_progress", activeForm: "b" },
    ] } })),
    toolUse("call-done", "Grep", { pattern: "x" }),
    toolResult("call-done"),
    toolUse("call-live", "Bash", { command: "pytest tests/ingest" }),
    perm("Edit", { file_path: "a.py" }),
    full(TranscriptEntryType.QUESTION, JSON.stringify({ questions: [{ question: "q?", header: "h", options: [] }] })),
  ];
  const s = listSummary(entries);
  expect(s.todos.length).toBe(2);
  expect(s.pendingPermission?.tool).toBe("Edit");
  expect(s.pendingQuestion).not.toBeNull();
  // The unresolved call, not the completed one before it.
  expect(s.inFlight?.tool).toBe("Bash");
  expect(s.inFlight?.summary).toContain("pytest");
});

test("listSummary reports no in-flight tool once every call has a result", () => {
  const entries = [toolUse("c1", "Grep"), toolResult("c1")];
  expect(listSummary(entries).inFlight).toBeNull();
});

test("inFlightTool prefers the SDK's own tool_progress elapsed time", () => {
  const call = toolUse("c1", "Bash", { command: "sleep 60" });
  const progress = full(
    TranscriptEntryType.SYSTEM,
    JSON.stringify({ sdk: "tool_progress", tool_use_id: "c1", elapsed_time_seconds: 32 }),
  );
  expect(inFlightTool([call, progress])?.elapsedSeconds).toBe(32);
});

test("entryTime returns null rather than a fake time for a transcript with no timestamp", () => {
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}"))).toBeNull();
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}", { createdAt: "not-a-date" }))).toBeNull();
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}", { createdAt: "2026-08-12T12:04:00Z" }))).not.toBeNull();
});

// What the dock keys off: it is the only surface a decision is answerable from
// now, so "is anything waiting on a human" has to be right for both shapes.
test("hasPendingDecision covers an unanswered permission and an unanswered question", () => {
  const waiting = perm("Edit", { file_path: "a.py" });
  expect(hasPendingDecision([waiting])).toBe(true);
  expect(hasPendingDecision([waiting, respond(waiting.seq, "allow")])).toBe(false);

  const asked = full(TranscriptEntryType.QUESTION, JSON.stringify({ questions: [{ question: "which?" }] }));
  expect(hasPendingDecision([asked])).toBe(true);
  expect(hasPendingDecision([asked, full(TranscriptEntryType.ANSWER, "{}")])).toBe(false);

  expect(hasPendingDecision([])).toBe(false);
});

// The three real turns that exposed this: cost/api-time climbed
// monotonically across turns whose own wall clock was 1m16s, 21s and 3.9s,
// so the last line claimed $1.546 and 7m24s of API time for four seconds
// of work.
test("resultDelta charges each turn only its own share of a cumulative total", () => {
  const first = {
    numTurns: 10,
    totalCostUsd: 1.425,
    durationMs: 76_000,
    durationApiMs: 422_000,
    modelUsage: { haiku: { costUSD: 0.068 }, sonnet: { costUSD: 1.357 } },
  };
  const second = {
    numTurns: 4,
    totalCostUsd: 1.52,
    durationMs: 21_000,
    durationApiMs: 440_000,
    modelUsage: { haiku: { costUSD: 0.073 }, sonnet: { costUSD: 1.448 } },
  };

  // No predecessor: the first result of a session is already its own share.
  expect(resultDelta(first, null)).toBe(first);

  const d = resultDelta(second, first);
  expect(d.totalCostUsd).toBeCloseTo(0.095, 5);
  expect(d.durationApiMs).toBe(18_000);
  expect(d.modelUsage?.sonnet.costUSD).toBeCloseTo(0.091, 5);
  // Per-result fields pass through untouched — they were never cumulative.
  expect(d.numTurns).toBe(4);
  expect(d.durationMs).toBe(21_000);
});

test("resultDelta clamps instead of going negative when a resumed session restarts its counters", () => {
  const before = { totalCostUsd: 1.546, durationApiMs: 444_000, modelUsage: { sonnet: { costUSD: 1.473 } } };
  const afterResume = { totalCostUsd: 0.02, durationApiMs: 3_000, modelUsage: { sonnet: { costUSD: 0.02 } } };

  const d = resultDelta(afterResume, before);
  expect(d.totalCostUsd).toBe(0);
  expect(d.durationApiMs).toBe(0);
  expect(d.modelUsage?.sonnet.costUSD).toBe(0);
});

// --- optimistic decisions ----------------------------------------------------

// Answering is two round trips (the RPC, then the entry arriving on the
// stream) before anything visibly changes. These cover the gap-filling: it
// must make the decision look answered everywhere at once, and it must lose
// to the server the instant the server has an opinion.
const optimisticResponse = (replyTo: bigint, behavior: "allow" | "deny") =>
  full(TranscriptEntryType.PERMISSION_RESPONSE, JSON.stringify({ behavior }), { from: "human", replyTo });

test("an optimistic response resolves the permission for every surface at once", () => {
  const request = perm("Edit", { file_path: "a.py" });
  const merged = withOptimistic([request], [optimisticResponse(request.seq, "allow")]);

  expect(findPendingPermissions(merged)).toEqual([]);
  expect(resolvedPermissionDecisions(merged).get(request.seq)).toBe("allow");
  expect(hasPendingDecision(merged)).toBe(false);
});

test("the real response supersedes the optimistic one instead of doubling it", () => {
  const request = perm("Edit");
  const real = respond(request.seq, "allow");
  const merged = withOptimistic([request, real], [optimisticResponse(request.seq, "allow")]);

  expect(merged.filter((e) => e.type === TranscriptEntryType.PERMISSION_RESPONSE).length).toBe(1);
  expect(merged).toEqual([request, real]);
});

test("an interrupt supersedes an unanswered optimistic response", () => {
  // The agent stopped waiting: showing "allowed" for a decision the fleet
  // never applied is the one failure worse than showing it late.
  const request = perm("Bash");
  const interrupt = full(TranscriptEntryType.INTERRUPT, "stopped");
  const merged = withOptimistic([request, interrupt], [optimisticResponse(request.seq, "allow")]);

  expect(merged.filter((e) => e.type === TranscriptEntryType.PERMISSION_RESPONSE).length).toBe(0);
  expect(resolvedPermissionDecisions(merged).get(request.seq)).toBe("interrupted");
});

test("an optimistic answer resolves its question, and the real answer replaces it", () => {
  const asked = full(TranscriptEntryType.QUESTION, JSON.stringify({ questions: [{ question: "which?" }] }));
  const echo = full(TranscriptEntryType.ANSWER, JSON.stringify({ answers: { "which?": "a" } }), {
    from: "human",
    replyTo: asked.seq,
  });
  expect(hasPendingDecision(withOptimistic([asked], [echo]))).toBe(false);

  const realAnswer = full(TranscriptEntryType.ANSWER, "{}", { from: "human", replyTo: asked.seq });
  const merged = withOptimistic([asked, realAnswer], [echo]);
  expect(merged.filter((e) => e.type === TranscriptEntryType.ANSWER).length).toBe(1);
});

test("withOptimistic returns the original array when it has nothing to add", () => {
  const request = perm("Edit");
  const real = respond(request.seq, "allow");
  const entries = [request, real];
  // Same reference, so React's own change detection sees no update.
  expect(withOptimistic(entries, [])).toBe(entries);
  expect(withOptimistic(entries, [optimisticResponse(request.seq, "allow")])).toBe(entries);
});

// --- subagents ----------------------------------------------------------------
// The SDK scatters one subagent run across five messages and two different
// correlation keys. These pin the joins, because the failure mode is silent:
// a run that fails to correlate still renders, just as a second half-empty row.

const agentCall = (id: string, subagentType: string, description: string) =>
  entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id, tool: "Agent", input: { subagent_type: subagentType, description } }));

const signal = (payload: Record<string, unknown>) =>
  entry(TranscriptEntryType.SYSTEM, "agent", JSON.stringify(payload));

test("subagentRuns joins an Agent call to its task_* stream", () => {
  const runs = subagentRuns([
    agentCall("toolu_1", "Explore", "Map dashboard feed rendering"),
    signal({ sdk: "task_started", task_id: "t1", tool_use_id: "toolu_1", subagent_type: "Explore" }),
  ]);
  expect(runs).toHaveLength(1);
  expect(runs[0]).toMatchObject({
    toolUseId: "toolu_1",
    subagentType: "Explore",
    description: "Map dashboard feed rendering",
    status: "running",
  });
});

// task_updated is the only message carrying the terminal status, and the only
// one that does NOT carry tool_use_id — so it resolves through the task_id
// index task_started built. Without that index a finished run shows "running"
// forever, which is the exact opposite of what the panel is for.
test("task_updated resolves through task_id alone", () => {
  const runs = subagentRuns([
    agentCall("toolu_1", "Explore", "d"),
    signal({ sdk: "task_started", task_id: "t1", tool_use_id: "toolu_1" }),
    signal({ sdk: "task_updated", task_id: "t1", patch: { status: "completed", end_time: 1787127340563 } }),
  ]);
  expect(runs[0].status).toBe("completed");
});

// …but an interim patch is not a terminal one. task_updated fires on every
// transition, and the tool_result promotion below only upgrades a run still
// marked running, so reading "in_progress" as failed paints it red forever.
test("an in-progress patch leaves the run running", () => {
  const runs = subagentRuns([
    agentCall("toolu_1", "Explore", "d"),
    signal({ sdk: "task_started", task_id: "t1", tool_use_id: "toolu_1" }),
    signal({ sdk: "task_updated", task_id: "t1", patch: { status: "in_progress" } }),
  ]);
  expect(runs[0].status).toBe("running");
});

test("a non-completed task status is a failure, not a completion", () => {
  const runs = subagentRuns([
    agentCall("toolu_1", "Explore", "d"),
    signal({ sdk: "task_notification", task_id: "t1", tool_use_id: "toolu_1", status: "error", summary: "boom" }),
  ]);
  expect(runs[0]).toMatchObject({ status: "failed", summary: "boom" });
});

// "it returned" and "it worked" are different claims: the tool_result only
// promotes a run that nothing else has judged.
test("the Agent tool_result completes a still-running run but cannot un-fail one", () => {
  const done = subagentRuns([
    agentCall("toolu_1", "Explore", "d"),
    entry(TranscriptEntryType.USER, "agent", JSON.stringify({ toolUseId: "toolu_1", content: "ok" })),
  ]);
  expect(done[0].status).toBe("completed");

  const failed = subagentRuns([
    agentCall("toolu_2", "Explore", "d"),
    signal({ sdk: "task_updated", task_id: "t2", patch: { status: "error" } }),
    signal({ sdk: "task_started", task_id: "t2", tool_use_id: "toolu_2" }),
    entry(TranscriptEntryType.USER, "agent", JSON.stringify({ toolUseId: "toolu_2", content: "ok" })),
  ]);
  expect(failed[0].status).toBe("failed");
});

// An Agent call is rendered by the AGENTS panel, so the feed's tool rows must
// not carry it too — same treatment TodoWrite has always had.
test("panel-owned tools never become tool-call pairs", () => {
  const pairs = buildToolCallPairs([
    agentCall("toolu_1", "Explore", "d"),
    entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_2", tool: "TodoWrite", input: { todos: [] } })),
    entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_3", tool: "Bash", input: { command: "ls" } })),
  ]);
  expect(pairs.map((p) => p.callInfo.tool)).toEqual(["Bash"]);
});

// The SDK's tool_progress tool_use_id is synthetic and suffixed; the real id
// is on parent_tool_use_id. Matching the former alone meant this lookup had
// never once succeeded, and every in-flight row fell through to wall-clock.
test("inFlightTool reads elapsed time from a heartbeat's parent_tool_use_id", () => {
  const flight = inFlightTool([
    entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_9", tool: "Bash", input: { command: "go test ./..." } })),
    signal({ sdk: "tool_progress", tool_use_id: "toolu_9-heartbeat-0", parent_tool_use_id: "toolu_9", tool_name: "Bash", elapsed_time_seconds: 90 }),
  ]);
  expect(flight).toMatchObject({ tool: "Bash", elapsedSeconds: 90 });
});

// The SDK calls a backgrounded Bash a "task" too, and in production it is the
// overwhelming majority: 162 of 186 task_started and 157 of 181
// task_notification point at a Bash tool_use, not an Agent one. Seeding a run
// from the signal put a bogus "agent" row with an empty description in the
// panel for every background command — ~87% of the rows it would have shown.
// Only an Agent tool_use we have actually seen makes a run.
test("a backgrounded Bash's task signals never become a subagent row", () => {
  const runs = subagentRuns([
    entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_bash", tool: "Bash", input: { command: "sleep 45", run_in_background: true } })),
    signal({ sdk: "task_started", task_id: "bw5d5o0s1", tool_use_id: "toolu_bash", description: "Wait briefly for explore agents" }),
    signal({ sdk: "task_notification", task_id: "bw5d5o0s1", tool_use_id: "toolu_bash", status: "completed", summary: 'Background command "Wait briefly" completed (exit code 0)' }),
  ]);
  expect(runs).toEqual([]);
});

// A summary is the subagent's actual answer, and the Agent call's tool_result
// is filtered out of the feed along with the call — so if the panel does not
// carry it, "what did it find" has no surface left in the console.
test("a completed run keeps the summary it came back with", () => {
  const runs = subagentRuns([
    agentCall("toolu_1", "Explore", "Map dashboard feed rendering"),
    signal({ sdk: "task_notification", task_id: "t1", tool_use_id: "toolu_1", status: "completed", summary: "found it in SignalView" }),
  ]);
  expect(runs[0]).toMatchObject({ status: "completed", summary: "found it in SignalView" });
});


// The CHANGES panel's rows carry git's repo-relative path; the SDK's file_path
// is absolute. Comparing them with === matched nothing, ever — and the failure
// is silent, because "no edits" is also the legitimate answer for a file a
// Bash command rewrote. So both halves get pinned here.
test("fileEdits: matches a repo-relative telemetry path against an absolute file_path", () => {
  const entries = [
    toolUse("e1", "Write", { file_path: "/workspace/app/src/a.ts", content: "one\n" }),
    toolUse("e2", "Bash", { command: "sed -i s/x/y/ src/b.ts" }),
    toolUse("e3", "Edit", { file_path: "/workspace/app/src/a.ts", old_string: "one", new_string: "two" }),
  ];

  const found = fileEdits(entries, "src/a.ts");
  expect(found.map((e) => e.tool)).toEqual(["Write", "Edit"]);

  // Oldest first: a file touched five times was five decisions, and the modal
  // renders them in the order they happened.
  expect(found[0].seq < found[1].seq).toBe(true);

  // An absolute path on both sides still matches.
  expect(fileEdits(entries, "/workspace/app/src/a.ts")).toHaveLength(2);

  // A file only a Bash command touched has no captured diff. This is the case
  // the modal explains rather than rendering an empty box.
  expect(fileEdits(entries, "src/b.ts")).toEqual([]);
});

// Suffix matching on a path boundary, not a bare endsWith.
test("fileEdits: does not match a path that is only a string suffix", () => {
  const entries = [toolUse("e1", "Edit", { file_path: "/workspace/app/not-a.ts", old_string: "x", new_string: "y" })];
  expect(fileEdits(entries, "a.ts")).toEqual([]);
});

// The one rule this whole change set exists to state, in the one place the UI
// reads it from. Inverting it is a live incident in either direction: dead
// allow/deny buttons on one side, an unanswerable-looking question on the other.
test("decisionAnswerable: a question outlives its pod, a permission does not", () => {
  const live = { podLive: true, swept: false };
  const dead = { podLive: false, swept: false };
  const swept = { podLive: false, swept: true };

  expect(decisionAnswerable("question", live)).toBe(true);
  expect(decisionAnswerable("permission", live)).toBe(true);

  // The asymmetry. A question's answer is a durable row and AnswerQuestion
  // warms a pod to deliver it (docs/adr/0050); a permission's allow/deny is
  // bound to the canUseTool promise of a pod that no longer exists.
  expect(decisionAnswerable("question", dead)).toBe(true);
  expect(decisionAnswerable("permission", dead)).toBe(false);

  // Swept is the exception on both counts: WarmIfIdle refuses the session, so
  // there is nothing to deliver an answer to.
  expect(decisionAnswerable("question", swept)).toBe(false);
  expect(decisionAnswerable("permission", swept)).toBe(false);
});

// --- background tasks ---------------------------------------------------------
// The trap this whole walk exists to avoid: a backgrounded Bash's tool_result
// comes back the INSTANT it is launched. Any promotion-on-tool_result rule (the
// one subagentRuns correctly uses) marks every background command finished at
// t=0 — the exact opposite of what a live-monitoring panel is for.

const bgCall = (id: string, command: string, description = "") =>
  entry(
    TranscriptEntryType.ASSISTANT,
    "agent",
    JSON.stringify({ id, tool: "Bash", input: { command, description, run_in_background: true } }),
  );

test("a backgrounded Bash is running until task_notification says otherwise", () => {
  const launched = [
    bgCall("toolu_bg", "bun install"),
    // The launch acknowledgement. NOT the outcome.
    entry(TranscriptEntryType.USER, "agent", JSON.stringify({ toolUseId: "toolu_bg", content: "started" })),
    signal({ sdk: "task_started", task_id: "b1", tool_use_id: "toolu_bg", description: "Install deps" }),
  ];
  expect(backgroundTasks(launched)).toEqual([
    { toolUseId: "toolu_bg", command: "bun install", description: "Install deps", status: "running" },
  ]);

  const done = backgroundTasks([
    ...launched,
    signal({ sdk: "task_notification", task_id: "b1", tool_use_id: "toolu_bg", status: "completed", summary: 'Background command "Install deps" completed (exit code 0)' }),
  ]);
  expect(done[0].status).toBe("completed");
  expect(done[0].summary).toContain("exit code 0");
});

// A non-backgrounded Bash is the overwhelming majority of Bash calls and has a
// feed row of its own. Seeding on tool name alone would put every `ls` in here.
test("a foreground Bash never becomes a background row", () => {
  expect(
    backgroundTasks([
      entry(TranscriptEntryType.ASSISTANT, "agent", JSON.stringify({ id: "toolu_fg", tool: "Bash", input: { command: "ls" } })),
      signal({ sdk: "task_started", task_id: "f1", tool_use_id: "toolu_fg" }),
    ]),
  ).toEqual([]);
});

// The mirror of subagentRuns' own guard. At signal level a subagent and a
// backgrounded Bash are indistinguishable, so each panel must require its own
// tool_use — otherwise the two panels render each other's rows.
test("a subagent's task stream never becomes a background row", () => {
  expect(
    backgroundTasks([
      agentCall("toolu_agent", "Explore", "map the feed"),
      signal({ sdk: "task_started", task_id: "a1", tool_use_id: "toolu_agent" }),
      signal({ sdk: "task_notification", task_id: "a1", tool_use_id: "toolu_agent", status: "completed" }),
    ]),
  ).toEqual([]);
});

// task_updated is the only terminal signal carrying no tool_use_id, same as in
// subagentRuns — it has to resolve through the task_id index.
test("task_updated resolves a background task through task_id alone", () => {
  const tasks = backgroundTasks([
    bgCall("toolu_bg", "go test ./..."),
    signal({ sdk: "task_started", task_id: "b2", tool_use_id: "toolu_bg" }),
    signal({ sdk: "task_updated", task_id: "b2", patch: { status: "error" } }),
  ]);
  expect(tasks[0].status).toBe("failed");
});

// An in-progress patch must not paint a healthy run red — task_updated fires on
// every transition, not only the last one.
test("an in-progress patch leaves a background task running", () => {
  const tasks = backgroundTasks([
    bgCall("toolu_bg", "sleep 45"),
    signal({ sdk: "task_started", task_id: "b3", tool_use_id: "toolu_bg" }),
    signal({ sdk: "task_updated", task_id: "b3", patch: { status: "in_progress" } }),
  ]);
  expect(tasks[0].status).toBe("running");
});

// Running first: a live command buried under four finished ones is the row the
// panel exists to show.
test("running background tasks sort above finished ones", () => {
  const tasks = backgroundTasks([
    bgCall("toolu_a", "first"),
    signal({ sdk: "task_notification", task_id: "ba", tool_use_id: "toolu_a", status: "completed" }),
    bgCall("toolu_b", "second"),
  ]);
  expect(tasks.map((t) => t.command)).toEqual(["second", "first"]);
});
