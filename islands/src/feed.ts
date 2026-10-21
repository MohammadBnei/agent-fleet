import { findPendingPermissions, findPendingQuestion, parseQuestions, type Question } from "../../dashboard/src/transcript";
import { TranscriptEntryType } from "../../dashboard/src/gen/agentfleet/v1/transcript_pb";
import type { Entry } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";
import { displayName } from "./text";

// What the island shows. The console's feed has tiers for tools, thinking and
// subagents; a host page gets the conversation and the decisions only.
export type Item =
  | { kind: "message"; seq: bigint; human: boolean; mine: boolean; author: string; text: string }
  | { kind: "question"; seq: bigint; questions: Question[] }
  | { kind: "permission"; seq: bigint; tool: string; input: unknown }
  | { kind: "plan"; seq: bigint; plan: string };

export function conversation(entries: Entry[]): Item[] {
  return entries
    .filter((e) => e.type === TranscriptEntryType.DISCUSSION && !e.contextOnly && (e.from === "human" || e.from === "agent"))
    .map((e) => ({
      kind: "message" as const,
      seq: e.seq,
      human: e.from === "human",
      // Set by core per caller (docs/adr/0067): the reader's own messages.
      mine: e.from === "human" && e.mine,
      author: e.from === "human" ? displayName(e.authorName) : "Agent",
      text: e.text,
    }));
}

// Open decisions, oldest first. ExitPlanMode is a plan, not a tool call.
export function decisions(entries: Entry[]): Item[] {
  const out: Item[] = [];
  const q = findPendingQuestion(entries);
  const qs = q ? parseQuestions(q.text) : null;
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


