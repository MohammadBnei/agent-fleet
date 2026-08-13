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
} from "./transcript";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, from = "agent", text = ""): TranscriptEntry {
  nextSeq += 1n;
  return { $typeName: "agentfleet.v1.TranscriptEntry", taskId: "t", seq: nextSeq, from, text, type };
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
    taskId: "t",
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
