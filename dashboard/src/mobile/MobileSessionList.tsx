import { useMemo, useState } from "react";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import {
  bucketSessions,
  isPodPhaseLive,
  sessionBadge,
  blockedForLabel,
  stuckLabel,
  SectionHeading,
  compareSessions,
  restStatus,
  TERMINAL,
  type SortKey,
} from "../pages/SessionList";
import type { ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { NotchCard } from "../components/NotchCard";
import { DecisionInline } from "../components/DecisionInline";
import { SessionActionsModal } from "../components/SessionActionsModal";
import { RowActionsButton } from "../components/RowControls";
import { QuietControls } from "../components/QuietControls";
import { useToggleSet } from "../useToggleSet";

// The phone list screen from Agent Fleet Console Mobile.dc.html. Not a
// narrowed copy of the desktop table: everything stacks, decisions are
// answerable inline with ~44px targets — this is the surface most likely used
// to unblock a session away from a desk (docs/dashboard-spec.md §8 item 5).
//
// The bucket chip row (needs you / stuck / working / done / all) is gone. It
// filtered which sections rendered, but the sections are already labelled,
// already ordered by urgency, and on a fleet of any realistic size they all fit
// one scroll — so it hid things without shortening anything, and defaulted to
// "needsYou", which meant a phone opened on an empty screen whenever nothing
// was blocked. Same sections, same order, always rendered: identical to
// desktop, which never had one.


function relativeTime(iso?: string): string | null {
  if (!iso) return null;
  const ms = Date.now() - new Date(iso).getTime();
  if (Number.isNaN(ms) || ms < 0) return null;
  const mins = Math.floor(ms / 60_000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function NeedsYouCard({
  session,
  summary,
  onSelect,
  onActions,
  reload,
  onAskLater,
}: {
  session: Session;
  summary?: ListSummary;
  onSelect: () => void;
  onActions: () => void;
  reload: () => void;
  onAskLater: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(session);
  const queued = summary?.pendingPermissionCount ?? 0;
  return (
    <NotchCard
      label={`◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}${queued > 1 ? ` · ${queued} decisions` : ""}`}
      tone="pink"
    >
      <div className="px-3.5 pt-3.5">
        <div className="flex items-baseline gap-2">
          <span className="text-xs text-dim2 min-w-0 truncate">{session.repo}</span>
          {todos.length > 0 && <TickBar todos={todos} blocked cell="w-[11px]" className="ml-auto flex-none" />}
          <RowActionsButton onOpen={onActions} inline />
        </div>
        <button
          type="button"
          onClick={onSelect}
          className="text-base leading-[1.55] mt-1.5 text-left w-full break-words cursor-pointer"
        >
          {sessionLabel(session)}
        </button>
      </div>
      <DecisionInline
        session={session}
        summary={summary}
        layout="stacked"
        reload={reload}
        onAskLater={onAskLater}
      />
    </NotchCard>
  );
}

function FinishedCard({
  session,
  onSelect,
  onActions,
  onOpenLogs,
  reload,
}: {
  session: Session;
  onSelect: () => void;
  onActions: () => void;
  onOpenLogs: () => void;
  reload: () => void;
}) {
  const failed = session.podPhase === "POD_PHASE_CRASHED";
  const when = relativeTime(session.lastActiveAt);
  return (
    <div className={`border px-3.5 py-3 ${failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"}`}>
      <div className="flex items-center gap-2">
        <span className={`w-1.5 h-1.5 rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        {when && <span className="text-xs text-dim2 ml-auto flex-none">{when}</span>}
        <RowActionsButton onOpen={onActions} inline />
      </div>
      <button
        type="button"
        onClick={onSelect}
        className="text-sm leading-[1.5] mt-1.5 text-left w-full break-words cursor-pointer"
      >
        {sessionLabel(session)}
      </button>
      {failed ? (
        <>
          <div className="text-xs text-warning mt-1.5 leading-[1.5] break-words">
            {"crashed"}
            {session.lastError ? ` · ${session.lastError}` : ""}
          </div>
          <div className="flex gap-2 mt-2.5">
            <button type="button" onClick={onOpenLogs} className="border border-acc-line px-3 py-2 text-xs flex-1">
              read log
            </button>
          </div>
        </>
      ) : (
        <div className="text-xs text-dim mt-1.5">
          {"no PR"}
        </div>
      )}
      {/* Same action as the desktop row's — see SessionList.tsx's FinishedRow. */}
      <button
        type="button"
        onClick={() => {
          void client.archiveSession({ sessionId: session.id }).then(reload);
        }}
        className="border border-acc-line px-3 py-2 text-xs w-full mt-2"
      >
        archive
      </button>
    </div>
  );
}

function WorkingCard({
  session,
  summary,
  last,
  onSelect,
  onActions,
}: {
  session: Session;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
  onActions: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = session.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(session.podPhase) && !provisioning;
  const stale = null as { label: string; className: string } | null;

  // A <div>, not a <button>: the card used to be one tap target end to end,
  // which makes the actions "⋯" a button inside a button — invalid HTML, and
  // React will not nest the click handlers sanely either. The id+label row is
  // the tap target now; everything below it is decoration that was never worth
  // tapping.
  return (
    <div className={`w-full text-left px-3.5 py-2.5 ${last ? "" : "border-b border-line3"}`}>
      <div className="flex items-center gap-2">
        <span
          className={`w-1.5 h-1.5 rounded-full flex-none ${
            stale ? "bg-error" : live ? "bg-info animate-fpulse" : "border border-dim2"
          }`}
        />
        <button type="button" onClick={onSelect} className="flex items-center gap-2 min-w-0 flex-1 text-left">
          <span className={`text-sm min-w-0 truncate ${live ? "" : "text-dim"}`}>{sessionLabel(session)}</span>
        </button>
        {session.permissionMode === "auto" ? (
          <span className="text-2xs text-warning border border-orange-line px-1.5 py-px ml-auto flex-none">
            auto
          </span>
        ) : (
          todos.length > 0 && <span className="text-xs text-dim2 ml-auto flex-none">{todoProgress(todos)}</span>
        )}
        <RowActionsButton onOpen={onActions} inline />
      </div>
      <div className="text-xs text-dim mt-1.5 truncate">
        {provisioning
          ? `booting${session.podMessage ? ` · ${session.podMessage}` : ""}`
          : inFlight
            ? `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${
                inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : ""
              }`
            : stale
              ? stale.label.toLowerCase()
              : session.liveState === "working"
                ? "working"
                : "idle"}
      </div>
      {provisioning ? (
        <div className="h-[3px] bar-provisioning mt-1.5" />
      ) : (
        todos.length > 0 && <TickBar todos={todos} className="mt-1.5" />
      )}
    </div>
  );
}

// One row of the quiet tail. Was four `Collapse` accordions (stalled / idle /
// archived / swept), which meant finding a dormant session required guessing
// which of them it had fallen into and clicking to check. Desktop had already
// replaced them with one flat sorted list for exactly that reason; this is the
// same list, using the same comparator, at phone width.
// Carries the ⋯ for the same reason CompactRow does: a fleet at rest is all
// quiet tail, so a row without it is a row you cannot act on for most of the
// console's life. No checkbox — batch actions are desktop-only, and a phone
// has neither the width for a checkbox column nor the use case for bulk tidying.
function QuietRow({
  session,
  onSelect,
  onActions,
}: {
  session: Session;
  onSelect: (id: string) => void;
  onActions: () => void;
}) {
  const badge = sessionBadge(session);
  return (
    <div className="flex items-center gap-2 py-2 border-b border-line3">
      <button
        type="button"
        onClick={() => onSelect(session.id)}
        className="flex items-center gap-2 text-left min-w-0 flex-1"
      >
        <span className="text-sm text-dim min-w-0 truncate flex-1">{sessionLabel(session)}</span>
        {badge && <span className={`text-2xs px-1 border tracking-wide flex-none ${badge.className}`}>{badge.label}</span>}
      </button>
      <RowActionsButton onOpen={onActions} inline />
    </div>
  );
}

// Same job as the desktop STUCK row, at phone width: a session burning
// wall-clock and asking nobody for anything. No allow/deny — the pod-gone case
// used to render exactly that in "needs you", where every click reached
// nothing.
function StuckCard({
  session,
  onSelect,
  onActions,
  onOpenLogs,
  reload,
}: {
  session: Session;
  onSelect: (id: string) => void;
  onActions: () => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
}) {
  const live = isPodPhaseLive(session.podPhase);
  const stuckFor = blockedForLabel(session);
  return (
    <div className="border border-orange-line bg-orange-bg px-3 py-2.5 flex flex-col gap-2">
      <div className="flex items-baseline gap-2 min-w-0">
        <button type="button" onClick={() => onSelect(session.id)} className="flex items-baseline gap-2 text-left min-w-0 flex-1">
          <span className="text-sm min-w-0 truncate flex-1">{sessionLabel(session)}</span>
        </button>
        <RowActionsButton onOpen={onActions} inline />
      </div>
      <span className="text-2xs text-warning">
        {stuckLabel(session)}
        {stuckFor ? ` \u00b7 ${stuckFor}` : ""}
      </span>
      <div className="flex gap-2">
        {live ? (
          <button
            type="button"
            onClick={() => void client.interrupt({ sessionId: session.id }).then(reload)}
            className="flex-1 border border-acc-line py-2 text-sm"
          >
            interrupt
          </button>
        ) : (
          session.sweptAt === undefined && (
            <button
              type="button"
              onClick={() => void client.warmSession({ sessionId: session.id }).then(reload)}
              className="flex-1 border border-acc-line py-2 text-sm"
            >
              warm
            </button>
          )
        )}
        <button type="button" onClick={() => onOpenLogs(session.id)} className="flex-1 border border-acc-line py-2 text-sm">
          read log
        </button>
      </div>
    </div>
  );
}

export function MobileSessionList({
  sessions,
  summaries,
  needsYouIds,
  onSelect,
  onDelete,
  onOpenLogs,
  reload,
}: {
  sessions: Session[];
  summaries: Map<string, ListSummary>;
  needsYouIds: Set<string>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
}) {
  // "ask me later" is per-session and deliberately not persisted: the agent is
  // still blocked, so the card must come back on the next visit.
  const [deferred, setDeferred] = useState<Set<string>>(new Set());
  // The quiet tail's two controls. Desktop's ControlBar has four; the repo
  // select and the status chips do not fit a phone, and the header's search box
  // already filters server-side, so they are deliberately not ported. The
  // comparator and the TERMINAL set are the desktop ones — those are the parts
  // that would drift.
  const [sort, setSort] = useState<SortKey>("date");
  const { set: hiddenRepos, toggle: toggleRepo } = useToggleSet();
  // Which card's "⋯" is open. Same shape and reasoning as SessionList's.
  const [actionsFor, setActionsFor] = useState<Session | null>(null);
  // Same model as desktop: a set of statuses to hide, seeded to the terminal
  // ones. See StatusFilter.
  const { set: hidden, toggle: toggleStatus } = useToggleSet(TERMINAL);

  // Same destructure as SessionList's, for the same reason: a bucket computed and
  // not rendered is a session that vanishes from this list. Mobile had dropped
  // `archived` and `swept` too, so a session a human had finished existed on
  // neither form factor.
  const { needsYou, stuck, working, finished, quiet, archived, swept } = bucketSessions(sessions, needsYouIds);
  const visibleNeedsYou = needsYou.filter((t) => !deferred.has(t.id));
  // Same derivation and same axes as the desktop ControlBar, so the two lists
  // cannot disagree about what "filtered by repo" means. Status chips are still
  // left off — four more toggles do not fit beside these two on a phone, and the
  // header search already narrows by text.
  const repos = useMemo(() => [...new Set(sessions.map((t) => t.repo))].sort(), [sessions]);
  // From the unfiltered tail on purpose: derived from the visible rows, a status
  // would vanish from the menu the moment you hid it, which is the same
  // one-way-door as above one level down.
  const restStatuses = useMemo(
    () => [...new Set([...quiet, ...archived, ...swept].map(restStatus))].sort(),
    [quiet, archived, swept],
  );
  // Two arrays, exactly as desktop has: `rest` is every quiet session and gates
  // the section, `visibleRest` is what survives the filters and gates only the
  // rows. Collapsing them into one — which this did — meant filtering
  // everything out removed the section, and the section contains the filter
  // controls, so unticking the last status hid the very selects needed to untick
  // it back. A filter must never be able to hide itself.
  const rest = [...quiet, ...archived, ...swept];
  const visibleRest = rest
    .filter((t) => !hiddenRepos.has(t.repo))
    .filter((t) => !hidden.has(restStatus(t)))
    .sort((a, b) => compareSessions(a, b, sort));



  return (
    <div className="flex-1 min-h-0 flex flex-col">
      <div className="flex-1 min-h-0 overflow-y-auto px-3.5 pt-4 pb-5 flex flex-col gap-3.5">
        {sessions.length === 0 && <div className="text-base text-dim">No sessions.</div>}

        {
          visibleNeedsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              session={t}
              onActions={() => setActionsFor(t)}
              summary={summaries.get(t.id)}
              onSelect={() => onSelect(t.id)}
              reload={reload}
              onAskLater={() => setDeferred((prev) => new Set(prev).add(t.id))}
            />
          ))}

        {finished.length > 0 && (
          <>
            <SectionHeading title="DONE WHILE AWAY" tone="green" className="mt-0.5" />
            {finished.map((t) => (
              <FinishedCard
                key={t.id}
                session={t}
                onActions={() => setActionsFor(t)}
                onSelect={() => onSelect(t.id)}
                onOpenLogs={() => onOpenLogs(t.id)}
                reload={reload}
              />
            ))}
          </>
        )}

        {stuck.length > 0 && (
          <>
            <SectionHeading title="STUCK" tone="pink" note="burning time, asking nobody" className="mt-0.5" />
            {stuck.map((t) => (
              <StuckCard
                key={t.id}
                session={t}
                onActions={() => setActionsFor(t)}
                onSelect={onSelect}
                onOpenLogs={onOpenLogs}
                reload={reload}
              />
            ))}
          </>
        )}

        {working.length > 0 && (
          <>
            <SectionHeading title="WORKING" className="mt-0.5" />
            <div className="border border-line2">
              {working.map((t, i) => (
                <WorkingCard
                  key={t.id}
                  session={t}
                  onActions={() => setActionsFor(t)}
                  summary={summaries.get(t.id)}
                  last={i === working.length - 1}
                  onSelect={() => onSelect(t.id)}
                />
              ))}
            </div>
          </>
        )}

        {/*
          "proposed by audits" was fed by a hardcoded empty array here too — a
          group that could never contain anything. Proposals are their own
          table with their own view on the Schedules page (docs/adr/0048).
        */}
        {rest.length > 0 && (
          <div className="flex flex-col mt-1">
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs tracking-[0.14em] text-dim2 whitespace-nowrap">QUIET</span>
              <span className="flex-1 h-px bg-line2" />
              <span className="text-xs text-dim2 whitespace-nowrap">
                {visibleRest.length} of {rest.length}
              </span>
            </div>
            <QuietControls
              sort={sort}
              setSort={setSort}
              repos={repos}
              hiddenRepos={hiddenRepos}
              toggleRepo={toggleRepo}
              statuses={restStatuses}
              hiddenStatuses={hidden}
              toggleStatus={toggleStatus}
              compact
            />
            {visibleRest.map((t) => (
              <QuietRow key={t.id} session={t} onSelect={onSelect} onActions={() => setActionsFor(t)} />
            ))}
            {visibleRest.length === 0 && (
              <span className="text-xs text-dim2 py-1">nothing matches these filters</span>
            )}
          </div>
        )}
      </div>

      <SessionActionsModal
        session={actionsFor}
        onClose={() => setActionsFor(null)}
        onDelete={onDelete}
        reload={reload}
      />
    </div>
  );
}
