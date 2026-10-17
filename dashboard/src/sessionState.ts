import type { Session } from "./gen/agentfleet/v1/core_pb";
import { sessionLabel } from "./sessionLabel";
import type { ListSummary } from "./transcript";

// What a session IS, derived from its row: which list bucket it belongs in,
// which badge it wears, what its working row says, how the quiet tail sorts
// it. Pure functions, no React — they lived in pages/SessionList.tsx, so the
// mobile list, the detail views, the actions menu and three test files all
// imported page code to reach them.

// A session with a live pod. Replaces ACTIVE_STATUSES, which named the three
// session statuses that meant "dispatched" — all gone with the enum
// (docs/adr/0048). Live state is derived from the row on every read, so this
// cannot drift from the transcript the way a cached status could.
export const LIVE_STATES = new Set(["working", "blocked", "idle", "stalled", "unknown"]);

// Shared section-membership split — used by both list views so the two never
// disagree about which bucket a session lands in.
//
// Every session must land in at least one bucket. SessionList.tsx carried a
// comment from whoever discovered that the hard way: a row matching no bucket
// vanishes from both lists, which is the one place a human is supposed to see
// it. bucketSessions.test.ts guards exactly this, and it is why `quiet` is
// defined as "everything not already claimed" rather than by its own
// predicate.
// `answerableIds` are sessions holding an unanswered QUESTION — computed
// client-side from the summaries App already fetches, because the wire carries
// one `pendingDecisions` count and cannot tell the two kinds apart.
//
// The distinction is the one docs/adr/0050 turns on. A question is durable and
// pod-independent: answering it warms a fresh pod and the answer is delivered
// there, so it belongs in NEEDS YOU whether or not a pod exists right now. A
// permission is bound to one live pod's canUseTool promise: with the pod gone
// its buttons reach nothing, so it belongs in STUCK, shown but not answerable.
export function bucketSessions(sessions: Session[], needsYouIds: Set<string>, answerableIds: Set<string> = new Set()) {
  const out = {
    needsYou: [] as Session[],
    stuck: [] as Session[],
    archived: [] as Session[],
    swept: [] as Session[],
    finished: [] as Session[],
    working: [] as Session[],
    quiet: [] as Session[],
  };

  // One pass with explicit precedence, rather than seven independent filters
  // that each re-derive which of the others they need to exclude.
  //
  // That shape had already drifted: `swept` excluded nothing, so a swept
  // session rendered under both "idle" and "swept", and a stalled session that
  // had been archived showed up twice as well. Each new bucket needed a new
  // exclusion added to every filter below it, and a missed one is a duplicate
  // row rather than an error.
  //
  // Assigning each session exactly once makes the buckets a partition by
  // construction: every session lands in one and only one, which is what the
  // list needs and what bucketSessions.test.ts asserts.
  for (const t of sessions) {
    // 1. A pending decision outranks everything: the session is stalled until
    //    someone clicks, and surfacing that is the product's whole job.
    //    pendingDecisions is a COUNT, not the old awaiting_human boolean —
    //    parallel tool calls each get their own permission, and answering one
    //    must not report the rest as resolved.
    //    Keyed on liveState, NOT on the raw pendingDecisions count. The
    //    server derives "blocked" as live-AND-has-pending, so the count alone
    //    disagrees with it by construction for a session whose pod is gone:
    //    that one would pin itself to the top of the list forever, offering
    //    allow/deny buttons that reach nothing.
    //
    //    That is not hypothetical — it is what `|| needsYouIds.has(t.id)` did
    //    here for the whole of the console's life, three lines under a comment
    //    describing the failure. The header census (App.tsx) counts liveState
    //    alone, so such a session sat at the top of the list under a header
    //    reading "0 waiting on you". It goes to `stuck` below instead: same
    //    visibility, and actions that can actually succeed.
    //    A dead pod holding an unanswered QUESTION belongs here too, and that
    //    is new: since AnswerQuestion warms, its allow/answer form is live
    //    again. Only a stranded PERMISSION drops through to `stuck` now.
    if (t.archivedAt === undefined && (t.liveState === "blocked" || answerableIds.has(t.id))) {
      out.needsYou.push(t);
      continue;
    }
    // 2. Burning wall-clock with nothing a human can usefully answer: a
    //    stalled session, or one whose pod died holding a PERMISSION — the
    //    question case was claimed above, because that one IS answerable.
    //    Above the fold, because the alternative was the quiet tail — and on
    //    mobile, a collapsed accordion behind a chip.
    if (t.archivedAt === undefined && (t.liveState === "stalled" || needsYouIds.has(t.id))) {
      out.stuck.push(t);
      continue;
    }
    // 3. The human already said they were finished with this one. Their
    //    statement outranks anything computed about it.
    if (t.archivedAt !== undefined) {
      out.archived.push(t);
      continue;
    }
    // 4. Disk reclaimed by the retention GC: readable, not resumable.
    //    Rendered without a Warm button — offering one would be an action
    //    that cannot succeed.
    if (t.sweptAt !== undefined) {
      out.swept.push(t);
      continue;
    }
    // 5. Finished-while-you-were-away: `done` liveness means the session
    //    completed and nobody has opened it since — opening marks it seen,
    //    which is what turns this back into idle. A crashed pod is always
    //    news, and with no `failed` status left it is the only thing that
    //    says so.
    if (t.liveState === "done" || t.podPhase === "POD_PHASE_CRASHED") {
      out.finished.push(t);
      continue;
    }
    if (t.liveState === "working") {
      out.working.push(t);
      continue;
    }
    // 6. Everything left: seen, idle, or dormant. The collapsed tail, and the
    //    catch-all that guarantees nothing falls through.
    out.quiet.push(t);
  }

  // Longest-waiting first, in the two buckets where waiting is the cost. The
  // list arrived in server order and never sorted, so a card that had been
  // blocked for 40 minutes could sit under one blocked for 40 seconds — order
  // is the only triage signal a list gives away for free, and it encoded
  // nothing. Sorted here, not at a render site, so both form factors and the
  // header's decisions modal inherit one answer.
  const oldestFirst = (a: Session, b: Session) => activeMs(a) - activeMs(b);
  out.needsYou.sort(oldestFirst);
  out.stuck.sort(oldestFirst);

  return out;
}


// Mirrors core/internal/sessions/store.go's IsPodPhaseLive — the client-side half
// of the same "does this session have a live pod right now" check the
// Warm/Discuss handlers use, so ActionsMenu can show Warm vs Stop without a
// round trip.
export function isPodPhaseLive(phase?: string): boolean {
  return phase === "POD_PHASE_PROVISIONING" || phase === "POD_PHASE_CREATED" || phase === "POD_PHASE_SCHEDULED" || phase === "POD_PHASE_RUNNING";
}

// ONE badge per session, chosen by precedence.
//
// Status, pod phase, liveness and staleness were four independently rendered
// badges of near-identical weight, and they overlap hard: a `claimed`/`running`
// status means "there is a live pod", which is what the pod badge already says;
// `unknown` liveness renders "STARTING", which is what
// `PROVISIONING`/`SCHEDULED` already says. Four badges that mostly restate each
// other read as noise, and the one that matters — a session waiting on a human —
// does not stand out from the three that don't.
//
// The order below is "what does a human need to know first", not the order the
// data happens to arrive in. Everything demoted out of the label survives in the
// tooltip, so nothing is lost, only ranked.
//
// Unchanged by the console rewrite, deliberately: docs/dashboard-spec.md §8
// item 1 asks for the ranking to be kept and given more visual weight, which is
// the callers' job, not this function's.
export function sessionBadge(session: Session): { label: string; className: string; title?: string } | null {
  const phase = session.podPhase?.replace("POD_PHASE_", "");

  // 1. Needs a human. Always wins: nothing else about this session matters
  //    until someone clicks, and this is the product's whole job.
  if (session.liveState === "blocked") {
    return { label: "NEEDS YOU", className: "text-error border-pink-line bg-pink-chip", title: "waiting on your decision" };
  }
  // 2. Something is wrong, in order of how wrong.
  if (phase === "CRASHED") {
    return { label: "CRASHED", className: "text-error border-pink-line bg-pink-chip", title: session.podMessage || session.lastError || undefined };
  }
  if (session.liveState === "stalled") {
    return { label: "STALLED", className: "text-warning border-orange-line bg-orange-bg", title: "no response since the last thing sent to the agent" };
  }
  // 3. Finished while nobody was looking — the "welcome back" state.
  if (session.liveState === "done") {
    return { label: "DONE", className: "text-success border-green-line bg-green-bg", title: "finished while you weren't looking — opening it marks it seen" };
  }
  // 4. Healthy and in motion. PROVISIONING keeps its sub-step inline: the
  //    difference between "cloning repo" and no event for 20 minutes is the
  //    exact gap that made a real incident invisible.
  if (phase === "PROVISIONING") {
    return {
      label: session.podMessage ? `PROVISIONING: ${session.podMessage}` : "PROVISIONING",
      className: "text-info border-info/45 bg-info/10 animate-pulse",
    };
  }
  if (session.liveState === "working") {
    return { label: "WORKING", className: "text-info border-info/45 bg-info/10", title: `pod ${phase?.toLowerCase() ?? "live"}` };
  }
  if (session.liveState === "unknown") {
    return { label: "STARTING", className: "text-info border-info/45 bg-info/10", title: "pod is up, the agent hasn't spoken yet" };
  }
  if (session.liveState === "idle") {
    return { label: "IDLE", className: "text-dim2 border-line bg-transparent", title: "pod live, nothing in flight" };
  }
  // 5. No live pod. The five workflow statuses that used to be rendered here
  //    (PROPOSED, QUEUED, FAILED, FAILED (final), CANCELLED, DONE) are gone
  //    with the enum (docs/adr/0048): there is no queue to be QUEUED in, no
  //    reclaim to be FAILED (final) from, and a proposal is a row in a
  //    different table with its own view. What is left is genuinely about the
  //    session rather than a workflow position.
  if (session.archivedAt) {
    return { label: "ARCHIVED", className: "text-dim2 border-line bg-transparent", title: "you marked this finished" };
  }
  if (session.sweptAt) {
    return {
      label: "SWEPT",
      className: "text-dim2 border-line bg-transparent",
      title: "working directory reclaimed by the retention GC — readable, not resumable",
    };
  }
  if (session.lastError) {
    return { label: "ERROR", className: "text-warning border-orange-line bg-orange-bg", title: session.lastError };
  }
  return null;
}

// What a working row says it is doing. One function for both form factors:
// the desktop row said "idle" for a session the mobile card called "working",
// on the same data, from the same bucket.
export function activityLabel(session: Session, inFlight: ListSummary["inFlight"]): string {
  if (inFlight) {
    const elapsed = inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : "";
    return `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${elapsed}`;
  }
  return session.liveState === "working" ? "working" : "idle";
}

// How long a session has been blocked — the notch label's "· 4m". This is the
// product's real cost (a blocked session is stalled until a human clicks), so
// it's stated on the card rather than left to be inferred from a heartbeat.
export function blockedForLabel(session: Session): string | null {
  if (!session.lastActiveAt) return null;
  const ms = Date.now() - new Date(session.lastActiveAt).getTime();
  if (Number.isNaN(ms) || ms < 0) return null;
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h`;
}

// The quiet tail's sort axes. Default `date` (most recently active first) is
// what a human scanning "what happened" wants; the rest are for hunting a
// specific session. proto Session has no createdAt, so date sorts on
// lastActiveAt with a 0 fallback for a session that never became active.
export type SortKey = "date" | "status" | "repo" | "title";

// The one bucket label shown per quiet session — also the status-filter axis.
export function restStatus(t: Session): string {
  if (t.archivedAt !== undefined) return "archived";
  if (t.sweptAt !== undefined) return "swept";
  // "stalled" is not an axis here any more — a stalled session is pinned in
  // STUCK above, never in the quiet tail, so a filter chip for it would only
  // ever match nothing.
  return "idle";
}

function activeMs(t: Session): number {
  return t.lastActiveAt ? new Date(t.lastActiveAt).getTime() : 0;
}

// Sort comparator for the quiet tail. Exported for sort.test.ts.
export function compareSessions(a: Session, b: Session, sort: SortKey): number {
  switch (sort) {
    case "repo":
      return a.repo.localeCompare(b.repo) || activeMs(b) - activeMs(a);
    case "title":
      return sessionLabel(a).localeCompare(sessionLabel(b)) || activeMs(b) - activeMs(a);
    case "status":
      return restStatus(a).localeCompare(restStatus(b)) || activeMs(b) - activeMs(a);
    default:
      return activeMs(b) - activeMs(a);
  }
}

// Archived and swept: the two states a human has already dealt with. Exported
// so mobile's quiet tail hides the same set — the desktop toggle and a
// hand-copied mobile one is exactly how the two lists drift.
export const TERMINAL = new Set(["archived", "swept"]);

// The one-line summary of why a session is in STUCK. Shared so the desktop row
// and the phone card cannot describe the same state differently — they each
// had their own copy of this ternary.
export function stuckLabel(session: Session): string {
  if (isPodPhaseLive(session.podPhase)) return "stalled";
  return session.pendingDecisions > 0 ? "pod gone, decision unanswerable" : "stalled";
}

// The notch over a NEEDS YOU card. One function so the desktop row and the
// phone card cannot word the same state differently — each built this string
// itself, and desktop also carried a dead "PROPOSED" branch from when a
// proposal was a session.
export function notchLabel(blockedFor: string | null, queued: number): string {
  return `◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}${queued > 1 ? ` · ${queued} decisions` : ""}`;
}
