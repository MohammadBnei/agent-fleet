import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";
import { isCogitating } from "./transcript";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, from = "agent"): TranscriptEntry {
  nextSeq += 1n;
  return { $typeName: "agentfleet.v1.TranscriptEntry", taskId: "t", seq: nextSeq, from, text: "", type };
}

test("isCogitating: false when pod isn't live", () => {
  expect(isCogitating([entry(TranscriptEntryType.DISCUSSION, "human")], false, false, false)).toBe(false);
});

test("isCogitating: false while a permission or question is pending", () => {
  const entries = [entry(TranscriptEntryType.DISCUSSION, "human")];
  expect(isCogitating(entries, true, true, false)).toBe(false);
  expect(isCogitating(entries, true, false, true)).toBe(false);
});

test("isCogitating: true right after a human message, before any result", () => {
  const entries = [entry(TranscriptEntryType.DISCUSSION, "human")];
  expect(isCogitating(entries, true, false, false)).toBe(true);
});

test("isCogitating: true after an answer or a permission response resumes the turn", () => {
  expect(isCogitating([entry(TranscriptEntryType.ANSWER, "human")], true, false, false)).toBe(true);
  expect(isCogitating([entry(TranscriptEntryType.PERMISSION_RESPONSE, "human")], true, false, false)).toBe(true);
});

test("isCogitating: false once a result entry closes the turn back to idle", () => {
  const entries = [entry(TranscriptEntryType.DISCUSSION, "human"), entry(TranscriptEntryType.RESULT)];
  expect(isCogitating(entries, true, false, false)).toBe(false);
});

test("isCogitating: false with no entries yet", () => {
  expect(isCogitating([], true, false, false)).toBe(false);
});
