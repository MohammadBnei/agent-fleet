import type { Entry } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";
import { TranscriptEntryType } from "../../dashboard/src/gen/agentfleet/v1/transcript_pb";
import {
  findPendingPermissions,
  findPendingQuestion,
  parseQuestions,
  type Question,
} from "../../dashboard/src/transcript";
import { answerText, displayNames } from "./text";

// What the island shows. The console's feed has tiers for tools, thinking and
// subagents; a host page gets the conversation and the decisions only.
export type Item =
  | {
      kind: "message";
      seq: bigint;
      human: boolean;
      mine: boolean;
      author: string;
      text: string;
    }
  | { kind: "question"; seq: bigint; questions: Question[] }
  | { kind: "permission"; seq: bigint; tool: string; input: unknown }
  | { kind: "plan"; seq: bigint; plan: string };

export function conversation(entries: Entry[]): Item[] {
  // A human's answer to a question is part of the conversation too: picked or typed.
  const said = entries.filter(
    (e) =>
      !e.contextOnly &&
      ((e.type === TranscriptEntryType.DISCUSSION &&
        (e.from === "human" || e.from === "agent")) ||
        (e.type === TranscriptEntryType.ANSWER && e.from === "human")),
  );
  const names = displayNames(
    said.filter((e) => e.from === "human").map((e) => e.authorName),
  );
  return said.map((e) => ({
    kind: "message" as const,
    seq: e.seq,
    human: e.from === "human",
    // Set by core per caller (docs/adr/0067): the reader's own messages.
    mine: e.from === "human" && e.mine,
    author:
      e.from === "human"
        ? (names.get(e.authorName) ?? "Another member")
        : "Agent",
    text: e.type === TranscriptEntryType.ANSWER ? answerText(e.text) : e.text,
  }));
}

// A stop or an interrupt ends the turn that asked: its question then blocks
// nothing, and its card would turn the next message into an answer nobody
// waits for. Agent text is no such signal: it can follow the ask in the turn.
function outlived(entries: Entry[], seq: bigint): boolean {
  return entries.some(
    (e) =>
      e.seq > seq &&
      (e.type === TranscriptEntryType.INTERRUPT ||
        e.type === TranscriptEntryType.ABORT),
  );
}

// Open decisions, oldest first. ExitPlanMode is a plan, not a tool call.
// `withdrawn` holds the request seqs closed unanswered in the decisions store
// (AnswerDecision "withdraw", the retention sweep): that leaves no transcript
// row, and a stamped request outlives an interrupt (docs/adr/0082 §5), so
// without it such a card stays open for good and every answer fails.
export function decisions(
  entries: Entry[],
  withdrawn: ReadonlySet<bigint> = new Set(),
): Item[] {
  const out: Item[] = [];
  const q = findPendingQuestion(entries);
  const qs = q && !outlived(entries, q.seq) ? parseQuestions(q.text) : null;
  if (q && qs) out.push({ kind: "question", seq: q.seq, questions: qs });
  for (const p of findPendingPermissions(entries)) {
    if (withdrawn.has(p.entry.seq)) continue;
    if (p.tool === "ExitPlanMode") {
      out.push({
        kind: "plan",
        seq: p.entry.seq,
        plan: String((p.input as { plan?: unknown } | undefined)?.plan ?? ""),
      });
    } else {
      out.push({
        kind: "permission",
        seq: p.entry.seq,
        tool: p.tool,
        input: p.input,
      });
    }
  }
  return out.sort((a, b) => (a.seq < b.seq ? -1 : 1));
}

// The header state. Blocked never overrides work (docs/adr/0082 §5, core's
// DeriveLiveState): a stamped request no longer stops the agent, so an open
// decision reads "waiting" only where the thread is not working, or while the
// request is still the last word (an older worker waits there until answered).
export function liveState(
  entries: Entry[],
  open: number,
  state: string,
): string {
  const last = entries.at(-1)?.type;
  const asking =
    last === TranscriptEntryType.PERMISSION_REQUEST ||
    last === TranscriptEntryType.QUESTION;
  return open && (state !== "working" || asking) ? "waiting" : state;
}
