import { useMemo, useState } from "react";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import { parseQuestions, summarizeToolInput, type ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { DecisionInline } from "../components/DecisionInline";
import { SessionActionsModal } from "../components/SessionActionsModal";
import { RowSelect, RowActionsButton, RowAction } from "../components/RowControls";
import { useSelection, BatchBar } from "../components/BatchActions";
import { QuietControls } from "../components/QuietControls";
import { useToggleSet } from "../useToggleSet";
import { NotchCard } from "../components/NotchCard";
import { AUTO_MODE_WARNING } from "../approvePlan";
import {
  activityLabel,
  blockedForLabel,
  bucketSessions,
  compareSessions,
  isPodPhaseLive,
  notchLabel,
  restStatus,
  sessionBadge,
  stuckLabel,
  TERMINAL,
  type SortKey,
} from "../sessionState";

// Neutral on purpose. Since docs/adr/0061 nearly every session launches in
// auto, so a warning-coloured badge on every row stopped meaning anything —
// and on mobile it took the slot the todo progress needed. The grant itself is
// confirmed where it is made (AutoModeConfirm); the tooltip repeats it.
export function AutoBadge({ className = "" }: { className?: string }) {
  return (
    <span className={`text-2xs text-dim2 border border-line px-1.5 py-px flex-none ${className}`} title={AUTO_MODE_WARNING}>
      auto
    </span>
  );
}


export function SectionHeading({
  title,
  tone = "dim",
  note,
  className = "",
}: {
  title: string;
  tone?: "pink" | "orange" | "green" | "dim";
  note?: string;
  className?: string;
}) {
  // orange: STUCK. Its rows are warning-orange, and a pink heading over them
  // borrowed the NEEDS YOU colour for a section a human cannot answer.
  const color =
    tone === "pink" ? "text-error" : tone === "orange" ? "text-warning" : tone === "green" ? "text-success" : "text-dim2";
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

  return (
    <div className="relative group">
      <NotchCard label={notchLabel(blockedFor, queued)}>
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
          <RowAction
            label="interrupt"
            action={() => client.interrupt({ sessionId: session.id })}
            reload={reload}
            className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
          />
        ) : (
          // Swept means the retention GC took the working directory: warming
          // would bring up a pod with nothing to resume, so it is not offered.
          session.sweptAt === undefined && (
            <RowAction
              label="warm"
              action={() => client.warmSession({ sessionId: session.id })}
              reload={reload}
              className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
            />
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
        {/* No "no PR" on the other branch: the fleet does not track PRs (the
            column went with adr/0048), so it said "no PR" on every row,
            including the ones that opened one. */}
        {failed ? (
          <span className="ml-auto text-sm text-warning min-w-0 truncate" title={session.lastError}>
            crashed{session.lastError ? ` · ${session.lastError}` : ""}
          </span>
        ) : (
          <span className="ml-auto" />
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
        <RowAction
          label="archive"
          action={() => client.archiveSession({ sessionId: session.id })}
          reload={reload}
          className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
        />
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

  return (
    <div className={`relative group ${last ? "" : "border-b border-line3"}`}>
      <div className="flex items-center gap-3 px-4 py-2.5 pr-[70px]">
        <span
          className={`w-[7px] h-[7px] rounded-full flex-none ${live ? "bg-info animate-fpulse" : "border border-dim2"}`}
        />
        <button
          type="button"
          onClick={onSelect}
          className={`text-base text-left hover:text-primary cursor-pointer min-w-0 max-w-[45%] shrink-0 truncate ${live ? "" : "text-dim"}`}
        >
          {sessionLabel(session)}
        </button>
        <span className="text-xs text-dim2 flex-none">{session.repo}</span>
        {session.permissionMode === "auto" && <AutoBadge />}

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
            <span className="ml-auto text-sm text-dim min-w-0 truncate">{activityLabel(session, inFlight)}</span>
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


export function SessionList({
  sessions,
  summaries,
  needsYouIds,
  answerableIds,
  onSelect,
  onDelete,
  onOpenLogs,
  reload,
  loaded = true,
}: {
  sessions: Session[];
  summaries: Map<string, ListSummary>;
  needsYouIds: Set<string>;
  answerableIds: Set<string>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
  // False until App's first poll answers: "No sessions." is a claim about the fleet.
  loaded?: boolean;
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
    return <div className="p-5 text-base text-dim">{loaded ? "No sessions." : "Loading…"}</div>;
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
          <SectionHeading title="DONE WHILE AWAY" tone="green" className="mt-4" />
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
          <SectionHeading title="STUCK" tone="orange" note="burning time, asking nobody" className="mt-4" />
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

