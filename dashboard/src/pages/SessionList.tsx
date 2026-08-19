import { useMemo, useState } from "react";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import { parseQuestions, summarizeToolInput, type ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { DecisionInline } from "../components/DecisionInline";
import { SessionActionsModal } from "../components/SessionActionsModal";
import { RowSelect, RowActionsButton } from "../components/RowControls";
import { useSelection, BatchBar } from "../components/BatchActions";
import { QuietControls } from "../components/QuietControls";
import { useToggleSet } from "../useToggleSet";
import { NotchCard } from "../components/NotchCard";

// A session with a live pod. Replaces ACTIVE_STATUSES, which named the three
// session statuses that meant "dispatched" — all gone with the enum
// (docs/adr/0048). Live state is derived from the row on every read, so this
// cannot drift from the transcript the way a cached status could.
const LIVE_STATES = new Set(["working", "blocked", "idle", "stalled", "unknown"]);
export const ACTIVE_STATES = LIVE_STATES;

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

// prBadge is gone with the pr_url column (docs/adr/0048), whose only writer
// never actually passed it — so this badge has been rendering `null` for every
// session in the fleet's history.
//
// The PR is discoverable without storing it: the agent names its own branch,
// and a GitHub search on that branch cannot go stale the way a column can.

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
  const stale = staleBadge(session);
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
  if (stale) return stale;
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

// staleBadge is gone with heartbeat_at. It fired when a heartbeat was 10
// minutes stale, mirroring the reclaim threshold — but there is no heartbeat
// and no reclaim now. The equivalent signal is CRASHED, written by the
// reconcile loop when Kubernetes says the pod is gone (docs/adr/0048).
function staleBadge(_: Session): { label: string; className: string; title?: string } | null {
  return null;
}

// heartbeatLabel is gone with heartbeat_at (docs/adr/0048). last_active_at
// is the honest replacement: it says when something actually happened,
// where a heartbeat only said a timer was still running.

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

export function SectionHeading({
  title,
  tone = "dim",
  note,
  className = "",
}: {
  title: string;
  tone?: "pink" | "green" | "dim";
  note?: string;
  className?: string;
}) {
  const color = tone === "pink" ? "text-error" : tone === "green" ? "text-success" : "text-dim2";
  return (
    <div className={`flex items-center gap-2.5 ${className}`}>
      <span className={`text-xs tracking-[0.14em] ${color} whitespace-nowrap`}>{title}</span>
      <span className={`flex-1 h-px ${tone === "pink" ? "bg-line" : "bg-line2"}`} />
      {note && <span className="text-xs text-dim2 whitespace-nowrap">{note}</span>}
    </div>
  );
}

function DeleteButton({ onDelete }: { onDelete: () => void }) {
  return (
    <button
      type="button"
      onClick={onDelete}
      title="Delete this session"
      aria-label="Delete this session"
      className="absolute top-1.5 right-1.5 w-5 h-5 flex items-center justify-center text-dim2 hover:text-error hover:bg-pink-chip text-xs"
    >
      ✕
    </button>
  );
}

// A blocked session, with its actual pending decision rendered inline. Before
// the rewrite this card said only "a decision is waiting" and made you open the
// session to find out which — the latency that costs is the product's whole
// point (docs/dashboard-spec.md §8 item 3).
function NeedsYouCard({
  session,
  onActions,
  picked,
  onPick,
  summary,
  onSelect,
  onDelete,
  reload,
}: {
  session: Session;
  onActions: () => void;
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
  summary?: ListSummary;
  onSelect: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(session);
  // The card answers one decision; saying so when there are more is what stops
  // "I answered it" from meaning "it is unblocked".
  const queued = summary?.pendingPermissionCount ?? 0;
  const isProposal = false;

  return (
    <div className="relative group">
      <NotchCard
        label={
          isProposal
            ? "◉ PROPOSED — NEEDS APPROVAL"
            : `◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}${queued > 1 ? ` · ${queued} decisions` : ""}`
        }
        tone="pink"
      >
        <div className="flex items-center gap-3 px-4 pt-3.5 pb-3 flex-wrap">
            <button
            type="button"
            onClick={onSelect}
            className="text-base text-left hover:text-primary cursor-pointer min-w-0 break-words"
          >
            {sessionLabel(session)}
          </button>
          <span className="text-xs text-dim2">{session.repo}</span>
          
          {todos.length > 0 && (
            <div className="ml-auto flex items-center gap-2">
              <TickBar todos={todos} blocked cell="w-4" />
              <span className="text-xs text-dim2">{todoProgress(todos)}</span>
            </div>
          )}
        </div>
        <DecisionInline session={session} summary={summary} reload={reload} />
      </NotchCard>
      <RowSelect picked={picked} onPick={onPick} />
      <RowActionsButton onOpen={onActions} />
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// A session burning wall-clock and asking nobody for anything: stalled, or a
// pod that died still holding a decision. Deliberately NOT allow/deny — the
// second kind used to render exactly that in the NEEDS YOU section, and every
// click reached a pod that no longer existed. What helps here is warming it
// back up, interrupting the turn it is wedged on, or reading why it stopped.
function StuckRow({
  session,
  summary,
  onActions,
  picked,
  onPick,
  onSelect,
  onOpenLogs,
  onDelete,
  reload,
}: {
  session: Session;
  summary?: ListSummary;
  onActions: () => void;
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
  onSelect: () => void;
  onOpenLogs: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const live = isPodPhaseLive(session.podPhase);
  const stuckFor = blockedForLabel(session);
  // What it was asking when the pod went. Read-only on purpose: allow/deny
  // would reach a pod that no longer exists. But hiding the question entirely
  // meant a session could ask something, lose its pod, and leave the operator
  // with no way to see what had been asked — reported live as "I can't see
  // questions now".
  const asked = summary?.pendingQuestion
    ? (parseQuestions(summary.pendingQuestion.text)?.[0]?.question ?? "a question")
    : summary?.pendingPermission
      ? `${summary.pendingPermission.tool} · ${summarizeToolInput(summary.pendingPermission.input)}`
      : null;
  return (
    <div className="relative group">
      <div className="flex flex-col gap-1.5 px-4 py-2.5 border border-orange-line bg-orange-bg pr-[70px]">
      <div className="flex items-center gap-3 flex-wrap">
        <span className="w-[7px] h-[7px] rounded-full flex-none bg-warning" />
        <button type="button" onClick={onSelect} className="text-base text-left hover:text-primary cursor-pointer min-w-0 break-words">
          {sessionLabel(session)}
        </button>
        <span className="text-xs text-dim2">{session.repo}</span>
        <span className="ml-auto text-sm text-warning min-w-0 truncate">
          {stuckLabel(session)}
          {stuckFor ? ` · ${stuckFor}` : ""}
        </span>
        {live ? (
          <button
            type="button"
            onClick={() => void client.interrupt({ sessionId: session.id }).then(reload)}
            className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
          >
            interrupt
          </button>
        ) : (
          // Swept means the retention GC took the working directory: warming
          // would bring up a pod with nothing to resume, so it is not offered.
          session.sweptAt === undefined && (
            <button
              type="button"
              onClick={() => void client.warmSession({ sessionId: session.id }).then(reload)}
              className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
            >
              warm
            </button>
          )
        )}
        <button type="button" onClick={onOpenLogs} className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary">
          read log
        </button>
      </div>
      {asked && (
        <div className="text-xs text-dim min-w-0">
          <span className="text-dim2">unanswered · </span>
          <span className="break-words">{asked}</span>
          {!live && session.sweptAt === undefined && (
            <span className="text-dim2"> — warm the session to answer it</span>
          )}
        </div>
      )}
      </div>
      <RowSelect picked={picked} onPick={onPick} />
      <RowActionsButton onOpen={onActions} />
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// One row of "finished while you were away" — green for a real result, orange
// for a failure. A failure gets the log that says why; the retry button that
// used to sit next to it is gone with docs/adr/0048's removal of
// failed_permanently — there is no dead state to resurrect a session from,
// and retrying is just sending it another message.
function FinishedRow({
  session,
  onActions,
  picked,
  onPick,
  onSelect,
  onOpenLogs,
  onDelete,
  reload,
}: {
  session: Session;
  onActions: () => void;
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
  onSelect: () => void;
  onOpenLogs: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const failed = session.podPhase === "POD_PHASE_CRASHED";
  const pr = null as { label: string; className: string } | null;
  return (
    <div className="relative group">
      <div
        className={`flex items-center gap-3 px-4 py-2.5 border pr-[70px] flex-wrap ${
          failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"
        }`}
      >
        <span className={`w-[7px] h-[7px] rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        <button type="button" onClick={onSelect} className="text-base text-left hover:text-primary cursor-pointer min-w-0 break-words">
          {sessionLabel(session)}
        </button>
        <span className="text-xs text-dim2">{session.repo}</span>
        {failed ? (
          <span className="ml-auto text-sm text-warning min-w-0 truncate" title={session.lastError}>
            {"crashed"}
            {session.lastError ? ` · ${session.lastError}` : ""}
          </span>
        ) : (
          <span className="ml-auto text-sm text-dim">{pr ? pr.label : "no PR"}</span>
        )}
        {failed ? (
          <button type="button" onClick={onOpenLogs} className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary">
            read log
          </button>
        ) : null}
        {/*
          This row is exactly the "I'm done with this" moment, and until now
          the only thing offered here was delete — which destroys the
          transcript, the sole record of a session that produced no PR.
          Archiving keeps the history and is the fleet's one terminal state.
        */}
        <button
          type="button"
          onClick={() => {
            void client.archiveSession({ sessionId: session.id }).then(reload);
          }}
          className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
        >
          archive
        </button>
      </div>
      <RowSelect picked={picked} onPick={onPick} />
      <RowActionsButton onOpen={onActions} />
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// The WORKING table: nothing here needs a human, so it's dense and calm. The
// live tool line is what makes "is it actually doing something" answerable
// without opening the session.
function WorkingRow({
  session,
  onActions,
  picked,
  onPick,
  summary,
  last,
  onSelect,
  onDelete,
}: {
  session: Session;
  onActions: () => void;
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = session.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(session.podPhase) && !provisioning;
  const stale = staleBadge(session);

  return (
    <div className={`relative group ${last ? "" : "border-b border-line3"}`}>
      <div className="flex items-center gap-3 px-4 py-2.5 pr-[70px]">
        <span
          className={`w-[7px] h-[7px] rounded-full flex-none ${
            stale ? "bg-error" : live ? "bg-info animate-fpulse" : "border border-dim2"
          }`}
        />
        <button
          type="button"
          onClick={onSelect}
          className={`text-base text-left hover:text-primary cursor-pointer min-w-0 truncate ${live ? "" : "text-dim"}`}
        >
          {sessionLabel(session)}
        </button>
        <span className="text-xs text-dim2 flex-none">{session.repo}</span>
        {session.permissionMode === "auto" && (
          <span
            className="text-xs text-warning border border-orange-line px-1.5 py-px flex-none"
            title="runs without asking — only rm and sudo still come to you"
          >
            auto
          </span>
        )}

        {provisioning ? (
          <>
            <span className="ml-auto text-sm text-dim2 min-w-0 truncate">
              booting{session.podMessage ? ` · ${session.podMessage}` : ""}
            </span>
            <span className="w-[105px] h-[3px] bar-provisioning flex-none" />
            <span className="text-xs text-dim2 w-6 text-right flex-none">—</span>
          </>
        ) : (
          <>
            <span className="ml-auto text-sm text-dim min-w-0 truncate">
              {inFlight
                ? `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${
                    inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : ""
                  }`
                : stale
                  ? stale.label.toLowerCase()
                  : "idle"}
            </span>
            <TickBar todos={todos} cell="w-4" className="flex-none" />
            <span className="text-xs text-dim2 w-6 text-right flex-none">
              {todos.length > 0 ? todoProgress(todos) : "—"}
            </span>
          </>
        )}
      </div>
      <RowSelect picked={picked} onPick={onPick} />
      <RowActionsButton onOpen={onActions} />
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// One flat row in the quiet tail. Was the inner markup of the old QuietGroup
// collapsibles, lifted out so the tail can be one sorted/filtered list instead
// of four fixed accordions.
//
// It carries the same checkbox and ⋯ as every other row, and that is a
// correction: the first version of this deliberately left both off, on the
// reasoning that "the quiet tail is where a human hunts one dormant session,
// not where they bulk-act". That was exactly backwards. A fleet at rest is ALL
// quiet tail — needsYou/working/finished are transient and often empty — so
// skipping this row meant a console with nothing selectable and nothing
// actionable on it for most of its life, which is what shipped in v4.8.0.
// Bulk-archiving a pile of dormant sessions is the single best use of a
// multi-select, and it was the one row that could not do it.
//
// A <div>, not a <button>: the row's own tap target cannot wrap the checkbox
// and the ⋯ (nested buttons are invalid HTML and swallow each other's clicks),
// so the id+label is the target and the controls sit beside it.
function CompactRow({
  session,
  onSelect,
  onActions,
  picked,
  onPick,
}: {
  session: Session;
  onSelect: (id: string) => void;
  onActions: () => void;
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
}) {
  const badge = sessionBadge(session);
  return (
    <div className="flex items-center gap-2.5 py-0.5 group">
      <button
        type="button"
        onClick={() => onSelect(session.id)}
        className="flex items-center gap-2.5 text-left hover:text-primary cursor-pointer min-w-0 flex-1"
      >
        <span className="text-sm text-dim min-w-0 truncate flex-1">{sessionLabel(session)}</span>
        <span className="text-2xs text-dim2 flex-none">{session.repo}</span>
        {badge && (
          <span className={`text-2xs px-1 border tracking-wide flex-none ${badge.className}`} title={badge.title}>
            {badge.label}
          </span>
        )}
      </button>
      {/* Inline, not the absolute corner overlay the taller cards use — this
          row is one line high, so there is no corner to park it in. */}
      <RowSelect picked={picked} onPick={onPick} inline />
      <RowActionsButton onOpen={onActions} inline />
    </div>
  );
}

// The quiet tail's sort axes. Default `date` (most recently active first) is
// what a human scanning "what happened" wants; the rest are for hunting a
// specific session. proto Session has no createdAt, so date sorts on
// lastActiveAt with a 0 fallback for a session that never became active.
export type SortKey = "date" | "status" | "repo" | "title";

// The one bucket label shown per quiet session — also the status-filter axis.
function restStatus(t: Session): string {
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

// The one bucket label a quiet session shows. Exported alongside TERMINAL for
// the same reason.
export { restStatus };


export function SessionList({
  sessions,
  summaries,
  needsYouIds,
  answerableIds,
  onSelect,
  onDelete,
  onOpenLogs,
  reload,
}: {
  sessions: Session[];
  summaries: Map<string, ListSummary>;
  needsYouIds: Set<string>;
  answerableIds: Set<string>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
}) {
  // Every bucket bucketSessions returns must be rendered somewhere below.
  //
  // `archived` and `swept` were computed and then dropped, which is the exact
  // failure the function's own comment warns about one level up: a session in
  // no RENDERED group vanishes from the list entirely. Archived sessions —
  // the only ones a human has explicitly finished — were invisible on both
  // form factors. bucketSessions.test.ts pins the coverage of the destructure so
  // the next bucket added cannot be silently dropped the same way.
  const { needsYou, stuck, working, finished, quiet, archived, swept } = bucketSessions(sessions, needsYouIds, answerableIds);
  // Which row's "⋯" is open, or null. The Session itself, not an id: the modal
  // reads podPhase/permissionMode/sweptAt off it, and re-looking-it-up by id on
  // every poll is how it would end up rendering a stale one.
  const [actionsFor, setActionsFor] = useState<Session | null>(null);
  const { selected, toggle, clear } = useSelection();

  // Quiet tail: one flat list over the four non-pinned buckets, sorted and
  // filtered by the ControlBar. Pinned-active (needsYou/finished/working) stays
  // above and is unaffected by any of this.
  const [sort, setSort] = useState<SortKey>("date");
  const { set: hiddenRepos, toggle: toggleRepo } = useToggleSet();
  // What to SHOW, replacing the old statusFilter ("empty means all", a sentinel
  // that read the same as "nothing selected") plus the separate hideTerminal
  // boolean, which said the same thing again more coarsely. Seeded to the
  // non-terminal statuses, which is what hideTerminal defaulted to expressing.
  const { set: hidden, toggle: toggleStatus } = useToggleSet(TERMINAL);

  const rest = useMemo(() => [...quiet, ...archived, ...swept], [quiet, archived, swept]);
  const repos = useMemo(() => [...new Set(sessions.map((t) => t.repo))].sort(), [sessions]);
  const statuses = useMemo(() => [...new Set(rest.map(restStatus))].sort(), [rest]);

  const visibleRest = useMemo(() => {
    return rest
      .filter((t) => !hiddenRepos.has(t.repo))
      .filter((t) => !hidden.has(restStatus(t)))
      .sort((a, b) => compareSessions(a, b, sort));
  }, [rest, hiddenRepos, hidden, sort]);

  // The four selectable sections, in the order they render — this is what a
  // shift-click range means, so it is built from the same arrays the body maps
  // over rather than from `sessions` (which is server order, not render order).
  // CompactRow is deliberately excluded: the quiet tail is where a human hunts
  // one dormant session, not where they bulk-act.
  // Render order, which is what a shift-click range means. visibleRest is
  // included: the quiet tail is the bulk of a resting fleet, and leaving it out
  // is what made the console look unchanged when nothing was active.
  const pickable = [...needsYou, ...finished, ...stuck, ...working, ...visibleRest];
  const orderedIds = pickable.map((t) => t.id);
  const pickedSessions = pickable.filter((t) => selected.has(t.id));
  const pick = (id: string) => (shiftKey: boolean) => toggle(id, shiftKey, orderedIds);

  if (sessions.length === 0) {
    return <div className="p-5 text-base text-dim">No sessions.</div>;
  }

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-4.5 pt-5 pb-6 flex flex-col gap-3">
      {/* Above everything, and only while something is selected — a permanently
          docked bar for an empty selection is a row of disabled buttons that
          teaches you to ignore that strip of the screen. */}
      <BatchBar sessions={pickedSessions} onClear={clear} reload={reload} />

      {needsYou.length > 0 && (
        <>
          <SectionHeading title="NEEDS YOU" tone="pink" className="mb-0.5" />
          {needsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              session={t}
              onActions={() => setActionsFor(t)}
              picked={selected.has(t.id)}
              onPick={pick(t.id)}
              summary={summaries.get(t.id)}
              onSelect={() => onSelect(t.id)}
              onDelete={() => onDelete(t.id)}
              reload={reload}
            />
          ))}
        </>
      )}

      {finished.length > 0 && (
        <>
          <SectionHeading title="FINISHED WHILE YOU WERE AWAY" tone="green" className="mt-4" />
          {finished.map((t) => (
            <FinishedRow
              key={t.id}
              session={t}
              onActions={() => setActionsFor(t)}
              picked={selected.has(t.id)}
              onPick={pick(t.id)}
              onSelect={() => onSelect(t.id)}
              onOpenLogs={() => onOpenLogs(t.id)}
              onDelete={() => onDelete(t.id)}
              reload={reload}
            />
          ))}
        </>
      )}

      {stuck.length > 0 && (
        <>
          <SectionHeading title="STUCK" tone="pink" note="burning time, asking nobody" className="mt-4" />
          {stuck.map((t) => (
            <StuckRow
              key={t.id}
              session={t}
              summary={summaries.get(t.id)}
              onActions={() => setActionsFor(t)}
              picked={selected.has(t.id)}
              onPick={pick(t.id)}
              onSelect={() => onSelect(t.id)}
              onOpenLogs={() => onOpenLogs(t.id)}
              onDelete={() => onDelete(t.id)}
              reload={reload}
            />
          ))}
        </>
      )}

      {working.length > 0 && (
        <>
          <SectionHeading title="WORKING" note="nothing needed from you" className="mt-4" />
          <div className="border border-line2">
            {working.map((t, i) => (
              <WorkingRow
                key={t.id}
                session={t}
                onActions={() => setActionsFor(t)}
                picked={selected.has(t.id)}
                onPick={pick(t.id)}
                summary={summaries.get(t.id)}
                last={i === working.length - 1}
                onSelect={() => onSelect(t.id)}
                onDelete={() => onDelete(t.id)}
              />
            ))}
          </div>
        </>
      )}

      {/*
        The quiet tail. Was four fixed collapsibles (stalled/idle/archived/
        swept); now one flat sorted+filtered list, so a human hunting a specific
        dormant session sorts by repo/title instead of guessing which accordion
        it fell into. Pinned-active above is untouched by these controls.
        "proposed by audits" used to sit here fed by a hardcoded empty array —
        proposals are their own table with their own Schedules view now
        (docs/adr/0048).
      */}
      {rest.length > 0 && (
        <div className="flex flex-col gap-2 mt-4">
          <div className="flex items-center gap-2.5">
            <span className="text-xs tracking-[0.14em] text-dim2 whitespace-nowrap">QUIET</span>
            <span className="flex-1 h-px bg-line2" />
            <span className="text-xs text-dim2 whitespace-nowrap">{visibleRest.length} of {rest.length}</span>
          </div>
          <QuietControls
            sort={sort}
            setSort={setSort}
            repos={repos}
            hiddenRepos={hiddenRepos}
            toggleRepo={toggleRepo}
            statuses={statuses}
            hiddenStatuses={hidden}
            toggleStatus={toggleStatus}
          />
          <div className="flex flex-col gap-0.5 pl-1 pt-1">
            {visibleRest.map((t) => (
              <CompactRow
                key={t.id}
                session={t}
                onSelect={onSelect}
                onActions={() => setActionsFor(t)}
                picked={selected.has(t.id)}
                onPick={pick(t.id)}
              />
            ))}
            {visibleRest.length === 0 && <span className="text-xs text-dim2 py-1">nothing matches these filters</span>}
          </div>
        </div>
      )}

      <SessionActionsModal
        session={actionsFor}
        onClose={() => setActionsFor(null)}
        onDelete={onDelete}
        reload={reload}
      />
    </div>
  );
}

// The one-line summary of why a session is in STUCK. Shared so the desktop row
// and the phone card cannot describe the same state differently — they each
// had their own copy of this ternary.
export function stuckLabel(session: Session): string {
  if (isPodPhaseLive(session.podPhase)) return "stalled";
  return session.pendingDecisions > 0 ? "pod gone, decision unanswerable" : "stalled";
}
