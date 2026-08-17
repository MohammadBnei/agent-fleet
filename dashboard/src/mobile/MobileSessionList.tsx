import { useState } from "react";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import {
  bucketSessions,
  isPodPhaseLive,

  sessionBadge,
  blockedForLabel,
  SectionHeading,
} from "../pages/SessionList";
import type { ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { NotchCard } from "../components/NotchCard";
import { DecisionInline } from "../components/DecisionInline";
import { Collapse } from "../components/Collapse";

// The phone list screen from Agent Fleet Console Mobile.dc.html. Not a
// narrowed copy of the desktop table: everything stacks, decisions are
// answerable inline with ~44px targets, and a bucket chip row replaces the
// desktop header's single filter box — this is the surface most likely used to
// unblock a session away from a desk (docs/dashboard-spec.md §8 item 5).

type Bucket = "needsYou" | "working" | "done" | "all";

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
  reload,
  onAskLater,
}: {
  session: Session;
  summary?: ListSummary;
  onSelect: () => void;
  reload: () => void;
  onAskLater: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(session);
  return (
    <NotchCard
      label={`◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}`}
      tone="pink"
    >
      <div className="px-3.5 pt-3.5">
        <div className="flex items-baseline gap-2">
          <span className="text-sm font-semibold">#{session.id.slice(0, 6)}</span>
          <span className="text-xs text-dim2 min-w-0 truncate">{session.repo}</span>
          {todos.length > 0 && <TickBar todos={todos} blocked cell="w-[11px]" className="ml-auto flex-none" />}
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
        onOpenSession={onSelect}
        reload={reload}
        onAskLater={onAskLater}
      />
    </NotchCard>
  );
}

function FinishedCard({
  session,
  onSelect,
  onOpenLogs,
  reload,
}: {
  session: Session;
  onSelect: () => void;
  onOpenLogs: () => void;
  reload: () => void;
}) {
  const failed = session.podPhase === "POD_PHASE_CRASHED";
  const when = relativeTime(session.lastActiveAt);
  return (
    <div className={`border px-3.5 py-3 ${failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"}`}>
      <div className="flex items-center gap-2">
        <span className={`w-1.5 h-1.5 rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        <span className="text-sm font-semibold">#{session.id.slice(0, 6)}</span>
        {when && <span className="text-xs text-dim2 ml-auto flex-none">{when}</span>}
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
}: {
  session: Session;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = session.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(session.podPhase) && !provisioning;
  const stale = null as { label: string; className: string } | null;

  return (
    <button
      type="button"
      onClick={onSelect}
      className={`w-full text-left px-3.5 py-2.5 ${last ? "" : "border-b border-line3"}`}
    >
      <div className="flex items-center gap-2">
        <span
          className={`w-1.5 h-1.5 rounded-full flex-none ${
            stale ? "bg-error" : live ? "bg-info animate-fpulse" : "border border-dim2"
          }`}
        />
        <span className={`text-sm flex-none ${live ? "text-text2" : "text-dim2"}`}>#{session.id.slice(0, 6)}</span>
        <span className={`text-sm min-w-0 truncate ${live ? "" : "text-dim"}`}>{sessionLabel(session)}</span>
        {session.permissionMode === "auto" ? (
          <span className="text-2xs text-warning border border-orange-line px-1.5 py-px ml-auto flex-none">
            auto
          </span>
        ) : (
          todos.length > 0 && <span className="text-xs text-dim2 ml-auto flex-none">{todoProgress(todos)}</span>
        )}
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
    </button>
  );
}

function QuietGroup({ title, sessions, onSelect }: { title: string; sessions: Session[]; onSelect: (id: string) => void }) {
  if (sessions.length === 0) return null;
  return (
    <Collapse
      summary={<span className="text-xs text-dim2">▸ {title} · {sessions.length}</span>}
      summaryClassName="py-1.5"
      contentClassName="pl-2 pb-1 flex flex-col"
    >
      {sessions.map((t) => {
        const badge = sessionBadge(t);
        return (
          <button
            key={t.id}
            type="button"
            onClick={() => onSelect(t.id)}
            className="flex items-center gap-2 text-left py-2"
          >
            <span className="text-xs text-dim2 flex-none">#{t.id.slice(0, 6)}</span>
            <span className="text-sm text-dim min-w-0 truncate flex-1">{sessionLabel(t)}</span>
            {badge && (
              <span className={`text-2xs px-1 border tracking-wide flex-none ${badge.className}`}>{badge.label}</span>
            )}
          </button>
        );
      })}
    </Collapse>
  );
}

export function MobileSessionList({
  sessions,
  summaries,
  needsYouIds,
  onSelect,
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
  const [bucket, setBucket] = useState<Bucket>("needsYou");
  // "ask me later" is per-session and deliberately not persisted: the agent is
  // still blocked, so the card must come back on the next visit.
  const [deferred, setDeferred] = useState<Set<string>>(new Set());

  // Same destructure as SessionList's, for the same reason: a bucket computed and
  // not rendered is a session that vanishes from this list. Mobile had dropped
  // `archived` and `swept` too, so a session a human had finished existed on
  // neither form factor.
  const { needsYou, working, finished, stalled, quiet, archived, swept } = bucketSessions(sessions, needsYouIds);
  const visibleNeedsYou = needsYou.filter((t) => !deferred.has(t.id));

  const CHIPS: readonly { value: Bucket; label: string; count: number }[] = [
    { value: "needsYou", label: "needs you", count: visibleNeedsYou.length },
    { value: "working", label: "working", count: working.length },
    { value: "done", label: "done", count: finished.length },
    { value: "all", label: "all", count: sessions.length },
  ];

  const showNeedsYou = bucket === "needsYou" || bucket === "all";
  const showFinished = bucket === "done" || bucket === "all";
  const showWorking = bucket === "working" || bucket === "all";

  return (
    <div className="flex-1 min-h-0 flex flex-col">
      <div className="flex-none flex gap-1.5 px-3.5 py-2 border-b border-line3 overflow-x-auto">
        {CHIPS.map((c) => (
          <button
            key={c.value}
            type="button"
            aria-pressed={bucket === c.value}
            onClick={() => setBucket(c.value)}
            className={`px-2.5 py-1 text-xs whitespace-nowrap border flex-none ${
              bucket === c.value
                ? c.value === "needsYou"
                  ? "border-pink-line bg-pink-chip text-error"
                  : "border-primary text-primary"
                : "border-line text-dim"
            }`}
          >
            {c.label} {c.count}
          </button>
        ))}
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto px-3.5 pt-4 pb-5 flex flex-col gap-3.5">
        {sessions.length === 0 && <div className="text-base text-dim">No sessions.</div>}

        {showNeedsYou &&
          visibleNeedsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              session={t}
              summary={summaries.get(t.id)}
              onSelect={() => onSelect(t.id)}
              reload={reload}
              onAskLater={() => setDeferred((prev) => new Set(prev).add(t.id))}
            />
          ))}

        {showFinished && finished.length > 0 && (
          <>
            <SectionHeading title="DONE WHILE AWAY" tone="green" className="mt-0.5" />
            {finished.map((t) => (
              <FinishedCard
                key={t.id}
                session={t}
                onSelect={() => onSelect(t.id)}
                onOpenLogs={() => onOpenLogs(t.id)}
                reload={reload}
              />
            ))}
          </>
        )}

        {showWorking && working.length > 0 && (
          <>
            <SectionHeading title="WORKING" className="mt-0.5" />
            <div className="border border-line2">
              {working.map((t, i) => (
                <WorkingCard
                  key={t.id}
                  session={t}
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
          table with their own view on the Audits page (docs/adr/0048).
        */}
        {bucket === "all" && (
          <div className="flex flex-col mt-1">
            <QuietGroup title="stalled" sessions={stalled} onSelect={onSelect} />
            <QuietGroup title="idle" sessions={quiet} onSelect={onSelect} />
            <QuietGroup title="archived" sessions={archived} onSelect={onSelect} />
            <QuietGroup title="swept · readable, not resumable" sessions={swept} onSelect={onSelect} />
          </div>
        )}
      </div>
    </div>
  );
}
