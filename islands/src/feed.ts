import { findPendingPermissions, findPendingQuestion, parseQuestions, type Question } from "../../dashboard/src/transcript";
import { TranscriptEntryType } from "../../dashboard/src/gen/agentfleet/v1/transcript_pb";
import type { Entry } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";
import { answerText, displayNames } from "./text";

// What the island shows. The console's feed has tiers for tools, thinking and
// subagents; a host page gets the conversation and the decisions only.
export type Item =
  | { kind: "message"; seq: bigint; human: boolean; mine: boolean; author: string; text: string }
  | { kind: "question"; seq: bigint; questions: Question[] }
  | { kind: "permission"; seq: bigint; tool: string; input: unknown }
  | { kind: "plan"; seq: bigint; plan: string };

export function conversation(entries: Entry[]): Item[] {
  // A human's answer to a question is part of the conversation too: picked or typed.
  const said = entries.filter(
    (e) =>
      !e.contextOnly &&
      ((e.type === TranscriptEntryType.DISCUSSION && (e.from === "human" || e.from === "agent")) ||
        (e.type === TranscriptEntryType.ANSWER && e.from === "human")),
  );
  const names = displayNames(said.filter((e) => e.from === "human").map((e) => e.authorName));
  return said
    .map((e) => ({
      kind: "message" as const,
      seq: e.seq,
      human: e.from === "human",
      // Set by core per caller (docs/adr/0067): the reader's own messages.
      mine: e.from === "human" && e.mine,
      author: e.from === "human" ? (names.get(e.authorName) ?? "Another member") : "Agent",
      text: e.type === TranscriptEntryType.ANSWER ? answerText(e.text) : e.text,
    }));
}

// A question no longer blocks once the turn that asked it ended: a stop, or the
// agent speaking again. Its card would otherwise turn the next message into an
// answer to a question nobody is waiting on.
function outlived(entries: Entry[], seq: bigint): boolean {
  return entries.some(
    (e) =>
      e.seq > seq &&
      (e.type === TranscriptEntryType.INTERRUPT ||
        e.type === TranscriptEntryType.ABORT ||
        (e.type === TranscriptEntryType.DISCUSSION && e.from === "agent")),
  );
}

// Open decisions, oldest first. ExitPlanMode is a plan, not a tool call.
export function decisions(entries: Entry[]): Item[] {
  const out: Item[] = [];
  const q = findPendingQuestion(entries);
  const qs = q && !outlived(entries, q.seq) ? parseQuestions(q.text) : null;
  if (q && qs) out.push({ kind: "question", seq: q.seq, questions: qs });
  for (const p of findPendingPermissions(entries)) {
    if (p.tool === "ExitPlanMode") {
      out.push({ kind: "plan", seq: p.entry.seq, plan: String((p.input as { plan?: unknown } | undefined)?.plan ?? "") });
    } else {
      out.push({ kind: "permission", seq: p.entry.seq, tool: p.tool, input: p.input });
    }
  }
  return out.sort((a, b) => (a.seq < b.seq ? -1 : 1));
}
