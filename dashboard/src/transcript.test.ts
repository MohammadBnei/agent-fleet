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
  spineItems,
  entryTime,
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

test("spineItems ranks a pending permission as pending and carries a denial's reason", () => {
  const allowed = perm("Bash", { command: "ls" });
  const denied = perm("WebFetch", { url: "http://x" });
  const waiting = perm("Edit", { file_path: "a.py" });
  const entries = [
    allowed,
    respond(allowed.seq, "allow"),
    denied,
    respond(denied.seq, "deny", "use the local fixture"),
    waiting,
  ];
  const items = spineItems(entries);
  expect(items.map((i) => i.kind)).toEqual(["allow", "deny", "pending"]);
  expect(items[0].label).toBe("allow · Bash");
  expect(items[1].detail).toBe("use the local fixture");
  expect(items[2].label).toBe("▸ permission · Edit");
  // The rail is a jump target: every item has to point at a real entry.
  expect(items[2].seq).toBe(waiting.seq);
});

test("spineItems renders an approved ExitPlanMode as a plan, not a permission", () => {
  const plan = perm("ExitPlanMode", { plan: "do the thing" });
  const items = spineItems([plan, respond(plan.seq, "allow")]);
  expect(items).toEqual([{ seq: plan.seq, kind: "plan", label: "plan approved", detail: null, time: null }]);
});

// A failed MCP server silently removes its tools from the session — the exact
// class of thing that makes a run go quiet for a reason nothing else reports.
test("spineItems raises an alarm for a non-connected mcp server and an sdk error", () => {
  const init = full(
    TranscriptEntryType.SYSTEM,
    JSON.stringify({ model: "opus", mcpServers: [{ name: "sidecar", status: "connected" }, { name: "playwright", status: "failed" }] }),
  );
  const err = full(TranscriptEntryType.SYSTEM, JSON.stringify({ sdk: "model_error", error: "rate limit" }));
  const items = spineItems([init, err]);
  expect(items.map((i) => i.label)).toEqual(["! mcp playwright failed", "! model_error"]);
  expect(items[1].detail).toBe("rate limit");
});

test("entryTime returns null rather than a fake time for a transcript with no timestamp", () => {
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}"))).toBeNull();
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}", { createdAt: "not-a-date" }))).toBeNull();
  expect(entryTime(full(TranscriptEntryType.ASSISTANT, "{}", { createdAt: "2026-08-12T12:04:00Z" }))).not.toBeNull();
});

// An interrupt denies every pending permission (worker/src/session.ts's
// resolveAllPendingDeny) without posting a PERMISSION_RESPONSE. The tool never
// ran, so the rail must not colour it like a grant.
test("spineItems groups an interrupted permission with deny, not allow", () => {
  const p = perm("Bash", { command: "rm -rf /" });
  const items = spineItems([p, full(TranscriptEntryType.INTERRUPT, "", { from: "human" })]);
  expect(items).toHaveLength(1);
  expect(items[0].kind).toBe("deny");
  expect(items[0].label).toBe("interrupted · Bash");
});
