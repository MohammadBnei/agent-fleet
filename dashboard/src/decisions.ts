import { agent } from "./connectClient";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "./gen/agentfleet/v1/agent_pb";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "./gen/agentfleet/v1/transcript_pb";
import { isPodPhaseLive, shortId } from "./sessionState";
import {
  decisionAnswerable,
  type ListSummary,
  listSummary,
  parseAnswers,
  summaryAnswerable,
} from "./transcript";

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
type ListApi = Pick<DecisionApi, "listDecisions">;

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

// The console deck's OPEN decisions, every session's.
// ponytail: one page of 200, newest first; past that the oldest open ones
// drop off the deck and its count. Page when a fleet ever holds that many.
export async function listOpenDecisions(
  api: ListApi = agent,
): Promise<Decision[]> {
  const res = await api.listDecisions({
    states: [DecisionState.OPEN],
    pageSize: 200,
  });
  return res.decisions;
}

// The deck's History tab: every closed decision, a page at a time.
export async function listDecisionHistory(
  pageToken: string,
  api: ListApi = agent,
): Promise<{ decisions: Decision[]; nextPageToken: string }> {
  const res = await api.listDecisions({
    states: [
      DecisionState.ANSWERED,
      DecisionState.WITHDRAWN,
      DecisionState.EXPIRED,
      DecisionState.STALE,
    ],
    pageSize: 50,
    pageToken,
  });
  return { decisions: res.decisions, nextPageToken: res.nextPageToken };
}

// One row of the deck. `decision` is unset only under the fallback (the store
// unavailable), where a blocked session's own transcript summary is the row.
export type DeckItem = {
  key: string;
  session: Session;
  summary?: ListSummary;
  decision?: Decision;
};

// The request as the transcript row DecisionInline already renders, so the
// deck answers with the same cards and the same stamped/plan detection.
// `interrupted` comes from the store: a single row has no INTERRUPT after it
// to derive it from, and faking one would be a lie in the data.
function summaryOf(d: Decision): ListSummary {
  const entry = {
    $typeName: "agentfleet.v1.TranscriptEntry",
    sessionId: d.threadId,
    seq: d.requestSeq,
    from: "agent",
    text: d.payloadJson,
    type:
      d.kind === DecisionKind.QUESTION
        ? TranscriptEntryType.QUESTION
        : TranscriptEntryType.PERMISSION_REQUEST,
    createdAt: d.createdAt,
  } as TranscriptEntry;
  const sum = listSummary([entry]);
  if (sum.pendingPermission) sum.pendingPermission.interrupted = d.interrupted;
  return sum;
}

// A session the list does not hold, named by its id.
function stubSession(d: Decision): Session {
  return {
    $typeName: "agentfleet.v1.Session",
    id: d.threadId,
    repo: d.subjectRepo,
    title: `#${shortId(d.threadId)}`,
  } as Session;
}

// The open decisions a human can act on, newest first. A question and a
// stamped request are durable (transcript.ts decisionAnswerable); an unstamped
// one is bound to its live pod, so it needs a known session with one, and an
// interrupted unstamped one has nothing left to answer. A durable one on a
// session the list does not hold (an admin's console or repo session) still
// shows, on a stub named by its id.
export function deckItems(open: Decision[], sessions: Session[]): DeckItem[] {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const out: DeckItem[] = [];
  for (const d of open) {
    // A card is durable by construction: it never pins a pod, and its answer
    // is core's message to the writer (docs/adr/0082). No transcript summary
    // either; CardBody reads the Decision itself.
    if (d.kind === DecisionKind.CARD) {
      const known = byId.get(d.threadId);
      if (known?.archivedAt !== undefined) continue;
      out.push({ key: d.id, session: known ?? stubSession(d), decision: d });
      continue;
    }
    const sum = summaryOf(d);
    const durable = summaryAnswerable(sum);
    const known = byId.get(d.threadId);
    // An archived session is out of needs-you (sessionState bucketSessions),
    // so its decision stays out too, or the LED, filter and list disagree.
    if (known?.archivedAt !== undefined) continue;
    if (
      known
        ? !decisionAnswerable(
            d.kind === DecisionKind.QUESTION ? "question" : "permission",
            {
              podLive: isPodPhaseLive(known.podPhase),
              swept: known.sweptAt !== undefined,
              stamped: durable,
            },
          )
        : !durable
    )
      continue;
    if (!durable && d.interrupted) continue;
    out.push({
      key: d.id,
      session: known ?? stubSession(d),
      summary: sum,
      decision: d,
    });
  }
  return out.sort(
    (a, b) =>
      Date.parse(b.decision?.createdAt ?? "") -
        Date.parse(a.decision?.createdAt ?? "") || 0,
  );
}

// A closed decision's outcome in a few words, for a History row.
export function answerLine(d: Decision): string {
  switch (d.state) {
    case DecisionState.WITHDRAWN:
      return "withdrawn";
    case DecisionState.EXPIRED:
      return "expired";
    case DecisionState.STALE:
      return "stale";
    case DecisionState.OPEN:
      return "open";
  }
  if (d.kind === DecisionKind.CARD) return cardAnswerLine(d);
  if (d.kind === DecisionKind.QUESTION) {
    const answers = parseAnswers(d.answerJson);
    return answers ? Object.values(answers).join(", ") : "answered";
  }
  let reply: { behavior?: unknown; message?: unknown };
  try {
    reply = JSON.parse(d.answerJson) ?? {};
  } catch {
    reply = {};
  }
  // The worker answers for itself only when a mode switch released the
  // request (worker/src/session.ts resolveOnePendingAllow).
  const auto = d.answeredBy === "worker" ? " (auto)" : "";
  if (reply.behavior === "allow")
    return (d.kind === DecisionKind.PLAN ? "approved" : "allowed") + auto;
  if (reply.behavior === "deny") {
    const msg = typeof reply.message === "string" ? reply.message.trim() : "";
    return msg ? `denied: ${msg}` : "denied";
  }
  return "answered";
}

// A card's answer_json is {card, optionId | ownText, answers}. The label comes
// from the card itself, not the answers text, which is core's message to the
// writer rather than words for a History row.
function cardAnswerLine(d: Decision): string {
  return cardChoiceLine(d.card?.options ?? [], d.answerJson);
}

// The owner's choice on a card in a few words, from the card's options and
// its answer_json. Shared by History and the session feed, which only has
// the card entry's JSON (protojson Card) rather than a Decision.
export function cardChoiceLine(
  options: readonly {
    id?: string;
    label?: string;
    effect?: { verb?: string };
  }[],
  answerJson: string,
): string {
  let reply: { optionId?: unknown; ownText?: unknown };
  try {
    reply = JSON.parse(answerJson) ?? {};
  } catch {
    reply = {};
  }
  if (reply.ownText === true) {
    const text = Object.values(parseAnswers(answerJson) ?? {})[0];
    return text ? `wrote: ${text}` : "wrote their own";
  }
  const option = options.find((o) => o.id === reply.optionId);
  if (!option) return "answered";
  const closed =
    option.effect?.verb === "close_threads" ? " · threads closed" : "";
  return `chose: ${option.label}${closed}`;
}
