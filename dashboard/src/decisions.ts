import { agent } from "./connectClient";
import { DecisionState } from "./gen/agentfleet/v1/agent_pb";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "./gen/agentfleet/v1/transcript_pb";

// The decisions store, for the one thing the transcript cannot say.
//
// Every surface derives a decision's state from the transcript (transcript.ts),
// and an answer is a transcript row. A withdrawn decision is not: core closes
// it and appends nothing (docs/adr/0082 §5), so from the transcript alone a
// closed request would stay pending forever. Each withdrawn decision becomes a
// synthetic "withdraw" reply here, which the derivations read like any other
// reply: no longer pending, outcome "withdrawn". Never rendered directly (the
// feed skips PERMISSION_RESPONSE rows).

// Seq is the request's own, so the reply sorts nowhere new; every derivation
// correlates replies by replyTo, never by position.
export function withdrawnReply(
  sessionId: string,
  seq: bigint,
): TranscriptEntry {
  return {
    $typeName: "agentfleet.v1.TranscriptEntry",
    sessionId,
    seq,
    from: "system",
    text: JSON.stringify({ behavior: "withdraw" }),
    type: TranscriptEntryType.PERMISSION_RESPONSE,
    replyTo: seq,
    createdAt: "",
  } as TranscriptEntry;
}

// `api` is a parameter for approvePlan's reason: a `mock.module` fake is
// process-wide in bun. Production callers omit it.
type DecisionApi = Pick<typeof agent, "listDecisions" | "answerDecision">;

// The session's withdrawn decisions as replies. Withdrawn never reopens, so a
// stale copy can only miss a closure, never hide an open request.
// ponytail: one page of 200, newest first; an older withdrawn one shows again
// as open (its close then fails as not open). Page when that is ever seen.
export async function fetchWithdrawn(
  sessionId: string,
  api: Pick<DecisionApi, "listDecisions"> = agent,
): Promise<TranscriptEntry[]> {
  try {
    const res = await api.listDecisions({
      threadId: sessionId,
      states: [DecisionState.WITHDRAWN],
      pageSize: 200,
    });
    return res.decisions.map((d) => withdrawnReply(sessionId, d.requestSeq));
  } catch {
    // The transcript alone is today's view; a failed read must not blank it.
    return [];
  }
}

// "Quick close": AnswerDecision "withdraw" closes the request unanswered,
// with no warm and no message (admin or the session's owner only). The
// transcript names a request by seq, the store by id, so the id is looked up
// at click time rather than held.
export async function withdrawDecision(
  sessionId: string,
  seq: bigint,
  api: DecisionApi = agent,
): Promise<void> {
  const res = await api.listDecisions({
    threadId: sessionId,
    states: [DecisionState.OPEN],
    pageSize: 200,
  });
  const decision = res.decisions.find((d) => d.requestSeq === seq);
  if (!decision) throw new Error("This request is no longer open.");
  await api.answerDecision({
    decisionId: decision.id,
    behavior: "withdraw",
  });
}
