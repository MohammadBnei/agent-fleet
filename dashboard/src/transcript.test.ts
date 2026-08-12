import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";
import {
  latestSlashCommands,
  latestSystemInfo,
  parseSdkSignal,
  parseSdkSystemInfo,
  permissionDenyMessages,
  buildToolCallPairs,
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
