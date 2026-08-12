import { useState } from "react";
import type { Task } from "../gen/agentfleet/v1/core_pb";
import { repoLabel } from "../taskKind";
import {
  bucketTasks,
  prBadge,
  isPodPhaseLive,
  staleBadge,
  sessionBadge,
  blockedForLabel,
  SectionHeading,
} from "../pages/TaskList";
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
  task,
  summary,
  onSelect,
  reload,
  onAskLater,
}: {
  task: Task;
  summary?: ListSummary;
  onSelect: () => void;
  reload: () => void;
  onAskLater: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(task);
  return (
    <NotchCard
      label={task.status === "proposed" ? "◉ PROPOSED" : `◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}`}
      tone="pink"
    >
      <div className="px-3.5 pt-3.5">
        <div className="flex items-baseline gap-2">
          <span className="text-[12.5px] font-semibold">#{task.id.slice(0, 6)}</span>
          <span className="text-[11px] text-dim2 min-w-0 truncate">{repoLabel(task)}</span>
          {todos.length > 0 && <TickBar todos={todos} blocked cell="w-[11px]" className="ml-auto flex-none" />}
        </div>
        <button
          type="button"
          onClick={onSelect}
          className="text-[13px] leading-[1.55] mt-1.5 text-left w-full break-words cursor-pointer"
        >
          {task.description}
        </button>
      </div>
      <DecisionInline
        task={task}
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
  task,
  onSelect,
  onRetry,
  onOpenLogs,
}: {
  task: Task;
  onSelect: () => void;
  onRetry: () => void;
  onOpenLogs: () => void;
}) {
  const failed = task.status === "failed" || task.status === "failed_permanently";
  const pr = prBadge(task);
  const when = relativeTime(task.lastActiveAt);
  return (
    <div className={`border px-3.5 py-3 ${failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"}`}>
      <div className="flex items-center gap-2">
        <span className={`w-1.5 h-1.5 rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        <span className="text-[12.5px] font-semibold">#{task.id.slice(0, 6)}</span>
        {when && <span className="text-[11px] text-dim2 ml-auto flex-none">{when}</span>}
      </div>
      <button
        type="button"
        onClick={onSelect}
        className="text-[12.5px] leading-[1.5] mt-1.5 text-left w-full break-words cursor-pointer"
      >
        {task.description}
      </button>
      {failed ? (
        <>
          <div className="text-[11.5px] text-warning mt-1.5 leading-[1.5] break-words">
            {task.status === "failed_permanently" ? "failed permanently" : "failed"}
            {task.lastError ? ` · ${task.lastError}` : ""}
          </div>
          <div className="flex gap-2 mt-2.5">
            <button type="button" onClick={onOpenLogs} className="border border-acc-line px-3 py-2 text-[11.5px] flex-1">
              read log
            </button>
            <button type="button" onClick={onRetry} className="border border-acc-line px-3 py-2 text-[11.5px] flex-1">
              retry
            </button>
          </div>
        </>
      ) : (
        <div className="text-[11.5px] text-dim mt-1.5">
          {pr ? pr.label : "no PR"}
          {task.prUrl && (
            <>
              {" · "}
              <a href={task.prUrl} target="_blank" rel="noreferrer" className="text-primary">
                review
              </a>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function WorkingCard({
  task,
  summary,
  last,
  onSelect,
}: {
  task: Task;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = task.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(task.podPhase) && !provisioning;
  const stale = staleBadge(task);

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
        <span className={`text-[12px] flex-none ${live ? "text-text2" : "text-dim2"}`}>#{task.id.slice(0, 6)}</span>
        <span className={`text-[12.5px] min-w-0 truncate ${live ? "" : "text-dim"}`}>{task.description}</span>
        {task.permissionMode === "bypassPermissions" ? (
          <span className="text-[10.5px] text-warning border border-orange-line px-1.5 py-px ml-auto flex-none">
            bypass
          </span>
        ) : (
          todos.length > 0 && <span className="text-[11px] text-dim2 ml-auto flex-none">{todoProgress(todos)}</span>
        )}
      </div>
      <div className="text-[11px] text-dim mt-1.5 truncate">
        {provisioning
          ? `booting${task.podMessage ? ` · ${task.podMessage}` : ""}`
          : inFlight
            ? `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${
                inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : ""
              }`
            : stale
              ? stale.label.toLowerCase()
              : task.liveState === "working"
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

function QuietGroup({ title, tasks, onSelect }: { title: string; tasks: Task[]; onSelect: (id: string) => void }) {
  if (tasks.length === 0) return null;
  return (
    <Collapse
      summary={<span className="text-[11.5px] text-dim2">▸ {title} · {tasks.length}</span>}
      summaryClassName="py-1.5"
      contentClassName="pl-2 pb-1 flex flex-col"
    >
      {tasks.map((t) => {
        const badge = sessionBadge(t);
        return (
          <button
            key={t.id}
            type="button"
            onClick={() => onSelect(t.id)}
            className="flex items-center gap-2 text-left py-2"
          >
            <span className="text-[11px] text-dim2 flex-none">#{t.id.slice(0, 6)}</span>
            <span className="text-[12px] text-dim min-w-0 truncate flex-1">{t.description}</span>
            {badge && (
              <span className={`text-[10px] px-1 border tracking-wide flex-none ${badge.className}`}>{badge.label}</span>
            )}
          </button>
        );
      })}
    </Collapse>
  );
}

export function MobileTaskList({
  tasks,
  summaries,
  needsYouIds,
  onSelect,
  onRetry,
  onOpenLogs,
  reload,
}: {
  tasks: Task[];
  summaries: Map<string, ListSummary>;
  needsYouIds: Set<string>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onRetry: (id: string) => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
}) {
  const [bucket, setBucket] = useState<Bucket>("needsYou");
  // "ask me later" is per-session and deliberately not persisted: the agent is
  // still blocked, so the card must come back on the next visit.
  const [deferred, setDeferred] = useState<Set<string>>(new Set());

  const { needsYou, working, finished, stalled, proposed, quiet } = bucketTasks(tasks, needsYouIds);
  const visibleNeedsYou = needsYou.filter((t) => !deferred.has(t.id));

  const CHIPS: readonly { value: Bucket; label: string; count: number }[] = [
    { value: "needsYou", label: "needs you", count: visibleNeedsYou.length },
    { value: "working", label: "working", count: working.length },
    { value: "done", label: "done", count: finished.length },
    { value: "all", label: "all", count: tasks.length },
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
            className={`px-2.5 py-1 text-[11.5px] whitespace-nowrap border flex-none ${
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
        {tasks.length === 0 && <div className="text-[13px] text-dim">No sessions.</div>}

        {showNeedsYou &&
          visibleNeedsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              task={t}
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
                task={t}
                onSelect={() => onSelect(t.id)}
                onRetry={() => onRetry(t.id)}
                onOpenLogs={() => onOpenLogs(t.id)}
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
                  task={t}
                  summary={summaries.get(t.id)}
                  last={i === working.length - 1}
                  onSelect={() => onSelect(t.id)}
                />
              ))}
            </div>
          </>
        )}

        {bucket === "all" && (
          <div className="flex flex-col mt-1">
            <QuietGroup title="stalled" tasks={stalled} onSelect={onSelect} />
            <QuietGroup title="proposed by audits" tasks={proposed} onSelect={onSelect} />
            <QuietGroup title="idle" tasks={quiet} onSelect={onSelect} />
          </div>
        )}
      </div>
    </div>
  );
}
