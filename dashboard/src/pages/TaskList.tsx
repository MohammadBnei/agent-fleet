import type { Task } from "../gen/agentfleet/v1/core_pb";
import { repoLabel } from "../taskKind";
import type { ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { Collapse } from "../components/Collapse";
import { DecisionInline } from "../components/DecisionInline";
import { NotchCard } from "../components/NotchCard";

export const ACTIVE_STATUSES = new Set(["pending", "claimed", "running"]);
export const SHIPPED_STATUSES = new Set(["done", "failed", "cancelled"]);
const FAILED_STATUSES = new Set(["failed", "failed_permanently"]);

// Shared section-membership split — used by both list views so the two never
// disagree about which bucket a task lands in.
//
// The original three keys are unchanged (mobile and bucketTasks.test.ts both
// depend on them); the console rewrite adds `finished` and `quiet`, splitting
// the old undifferentiated "shipped" pile into the mockups' two sections:
// "finished while you were away" (which is news) and the collapsed idle tail
// (which isn't).
export function bucketTasks(tasks: Task[], needsYouIds: Set<string>) {
  const needsYou = tasks.filter(
    // A 'proposed' task is by definition a decision waiting on a human (an
    // alert or the audit scheduler created it and nobody has approved it), so
    // it belongs here regardless of needsYouIds — that set tracks outstanding
    // transcript decisions, and a proposal has no transcript yet.
    //
    // Deliberately NOT added to ACTIVE_STATUSES: App.tsx uses that same set for
    // the per-task summary fetch and the live counters, and a proposal is
    // neither. Without this line a proposal matches no bucket at all and
    // vanishes from both lists — the one place a human is supposed to see it.
    (t) => t.status === "proposed" || (ACTIVE_STATUSES.has(t.status) && needsYouIds.has(t.id)),
  );
  const working = tasks.filter((t) => ACTIVE_STATUSES.has(t.status) && !needsYouIds.has(t.id));
  return {
    needsYou,
    working,
    shipped: tasks.filter((t) => SHIPPED_STATUSES.has(t.status)),
    // Finished-while-you-were-away: `done` liveness means it completed and
    // nobody has opened it since (opening marks it seen, which is what turns
    // this back into idle), and a failure is always news.
    finished: tasks.filter((t) => t.liveState === "done" || FAILED_STATUSES.has(t.status)),
    stalled: tasks.filter((t) => t.liveState === "stalled"),
    proposed: tasks.filter((t) => t.status === "proposed"),
    // Everything left: seen, finished, or dormant. The collapsed tail.
    quiet: tasks.filter(
      (t) =>
        !needsYou.includes(t) &&
        !working.includes(t) &&
        t.liveState !== "done" &&
        t.liveState !== "stalled" &&
        !FAILED_STATUSES.has(t.status),
    ),
  };
}

export function prBadge(task: Task): { label: string; className: string } | null {
  if (!task.prUrl) return null;
  const match = /\/pull\/(\d+)/.exec(task.prUrl);
  const number = match ? match[1] : null;
  if (task.status === "failed") {
    return { label: number ? `PR ${number} !` : "PR !", className: "text-warning" };
  }
  return { label: number ? `PR ${number} ✓` : "PR ✓", className: "text-success" };
}

// Mirrors core/internal/tasks/store.go's IsPodPhaseLive — the client-side half
// of the same "does this session have a live pod right now" check the
// Warm/Discuss handlers use, so ActionsMenu can show Warm vs Stop without a
// round trip.
export function isPodPhaseLive(phase?: string): boolean {
  return phase === "POD_PHASE_PROVISIONING" || phase === "POD_PHASE_CREATED" || phase === "POD_PHASE_SCHEDULED" || phase === "POD_PHASE_RUNNING";
}

// Matches core's own reclaim threshold (tasks.Store.ClaimNextTask reclaims a
// claimed/running task once its heartbeat is this stale) — the dashboard's
// "stuck" signal should fire at exactly the point a human can no longer
// distinguish "about to be reclaimed" from "silently wedged forever" (see the
// incident where a task sat claimed with no pod for 20+ minutes and nothing in
// the UI showed it).
const STALE_THRESHOLD_MS = 10 * 60 * 1000;

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
export function sessionBadge(task: Task): { label: string; className: string; title?: string } | null {
  const stale = staleBadge(task);
  const phase = task.podPhase?.replace("POD_PHASE_", "");

  // 1. Needs a human. Always wins: nothing else about this session matters
  //    until someone clicks, and this is the product's whole job.
  if (task.liveState === "blocked") {
    return { label: "NEEDS YOU", className: "text-error border-pink-line bg-pink-chip", title: "waiting on your decision" };
  }
  // 2. Something is wrong, in order of how wrong.
  if (phase === "CRASHED") {
    return { label: "CRASHED", className: "text-error border-pink-line bg-pink-chip", title: task.podMessage || task.lastError || undefined };
  }
  if (stale) return stale;
  if (task.liveState === "stalled") {
    return { label: "STALLED", className: "text-warning border-orange-line bg-orange-bg", title: "no response since the last thing sent to the agent" };
  }
  // 3. Finished while nobody was looking — the "welcome back" state.
  if (task.liveState === "done") {
    return { label: "DONE", className: "text-success border-green-line bg-green-bg", title: "finished while you weren't looking — opening it marks it seen" };
  }
  // 4. Healthy and in motion. PROVISIONING keeps its sub-step inline: the
  //    difference between "cloning repo" and no event for 20 minutes is the
  //    exact gap that made a real incident invisible.
  if (phase === "PROVISIONING") {
    return {
      label: task.podMessage ? `PROVISIONING: ${task.podMessage}` : "PROVISIONING",
      className: "text-info border-info/45 bg-info/10 animate-pulse",
    };
  }
  if (task.liveState === "working") {
    return { label: "WORKING", className: "text-info border-info/45 bg-info/10", title: `pod ${phase?.toLowerCase() ?? "live"}` };
  }
  if (task.liveState === "unknown") {
    return { label: "STARTING", className: "text-info border-info/45 bg-info/10", title: "pod is up, the agent hasn't spoken yet" };
  }
  if (task.liveState === "idle") {
    return { label: "IDLE", className: "text-dim2 border-line bg-transparent", title: "pod live, nothing in flight" };
  }
  // 5. No live pod: the workflow status is the only thing left to say, and now
  //    it has the badge to itself instead of competing with three others.
  if (task.status === "proposed") {
    return { label: "PROPOSED", className: "text-primary border-acc-line bg-primary/10", title: "machine-created — approve it to dispatch" };
  }
  if (task.status === "pending") {
    return { label: "QUEUED", className: "text-dim2 border-line bg-transparent" };
  }
  if (task.status === "failed" || task.status === "failed_permanently") {
    return { label: task.status === "failed" ? "FAILED" : "FAILED (final)", className: "text-warning border-orange-line bg-orange-bg", title: task.lastError || undefined };
  }
  if (task.status === "cancelled") {
    return { label: "CANCELLED", className: "text-warning border-orange-line bg-orange-bg" };
  }
  if (task.status === "done") {
    return { label: "DONE", className: "text-success border-green-line bg-green-bg" };
  }
  return null;
}

export function staleBadge(task: Task): { label: string; className: string; title?: string } | null {
  if (!ACTIVE_STATUSES.has(task.status) || !task.heartbeatAt) return null;
  const elapsedMs = Date.now() - new Date(task.heartbeatAt).getTime();
  if (elapsedMs < STALE_THRESHOLD_MS) return null;
  const minutes = Math.floor(elapsedMs / 60_000);
  return {
    label: `STALE ${minutes}m`,
    className: "text-error border-pink-line bg-pink-chip animate-pulse",
    title: task.lastError || `no heartbeat for ${minutes}m — will be reclaimed and redispatched`,
  };
}

export function heartbeatLabel(task: Task): string | null {
  if (!task.heartbeatAt) return null;
  const minutes = Math.floor((Date.now() - new Date(task.heartbeatAt).getTime()) / 60_000);
  return minutes < 1 ? "heartbeat just now" : `heartbeat ${minutes}m ago`;
}

// How long a session has been blocked — the notch label's "· 4m". This is the
// product's real cost (a blocked session is stalled until a human clicks), so
// it's stated on the card rather than left to be inferred from a heartbeat.
export function blockedForLabel(task: Task): string | null {
  if (!task.lastActiveAt) return null;
  const ms = Date.now() - new Date(task.lastActiveAt).getTime();
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
      <span className={`text-[11px] tracking-[0.14em] ${color} whitespace-nowrap`}>{title}</span>
      <span className={`flex-1 h-px ${tone === "pink" ? "bg-line" : "bg-line2"}`} />
      {note && <span className="text-[11px] text-dim2 whitespace-nowrap">{note}</span>}
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
      className="absolute top-1.5 right-1.5 w-5 h-5 flex items-center justify-center text-dim2 hover:text-error hover:bg-pink-chip text-[11px]"
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
  task,
  summary,
  onSelect,
  onDelete,
  reload,
}: {
  task: Task;
  summary?: ListSummary;
  onSelect: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(task);
  const isProposal = task.status === "proposed";

  return (
    <div className="relative">
      <NotchCard
        label={isProposal ? "◉ PROPOSED — NEEDS APPROVAL" : `◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}`}
        tone="pink"
      >
        <div className="flex items-center gap-3 px-4 pt-3.5 pb-2.5 flex-wrap">
          <span className="text-[13px] font-semibold">#{task.id.slice(0, 6)}</span>
          <button
            type="button"
            onClick={onSelect}
            className="text-[13.5px] text-left hover:text-primary cursor-pointer min-w-0 break-words"
          >
            {task.description}
          </button>
          <span className="text-[11.5px] text-dim2">{repoLabel(task)}</span>
          <span className="text-[11px] text-dim2 border border-line px-1.5 py-px">{task.status}</span>
          {todos.length > 0 && (
            <div className="ml-auto flex items-center gap-2">
              <TickBar todos={todos} blocked cell="w-4" />
              <span className="text-[11px] text-dim2">{todoProgress(todos)}</span>
            </div>
          )}
        </div>
        <DecisionInline task={task} summary={summary} onOpenSession={onSelect} reload={reload} />
      </NotchCard>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// One row of "finished while you were away" — green for a real result, orange
// for a failure. A failure gets the two things a human actually needs next:
// the log that says why, and a retry.
function FinishedRow({
  task,
  onSelect,
  onRetry,
  onOpenLogs,
  onDelete,
}: {
  task: Task;
  onSelect: () => void;
  onRetry: () => void;
  onOpenLogs: () => void;
  onDelete: () => void;
}) {
  const failed = FAILED_STATUSES.has(task.status);
  const pr = prBadge(task);
  return (
    <div className="relative">
      <div
        className={`flex items-center gap-3 px-4 py-2.5 border pr-8 flex-wrap ${
          failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"
        }`}
      >
        <span className={`w-[7px] h-[7px] rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        <span className="text-[13px] font-semibold">#{task.id.slice(0, 6)}</span>
        <button type="button" onClick={onSelect} className="text-[13.5px] text-left hover:text-primary cursor-pointer min-w-0 break-words">
          {task.description}
        </button>
        <span className="text-[11.5px] text-dim2">{repoLabel(task)}</span>
        {failed ? (
          <span className="ml-auto text-[12px] text-warning min-w-0 truncate" title={task.lastError}>
            {task.status === "failed_permanently" ? "failed permanently" : "failed"}
            {task.lastError ? ` · ${task.lastError}` : ""}
          </span>
        ) : (
          <span className="ml-auto text-[12px] text-dim">{pr ? pr.label : "no PR"}</span>
        )}
        {failed ? (
          <>
            <button type="button" onClick={onOpenLogs} className="flex-none border border-acc-line px-3 py-1 text-[12px] hover:border-primary hover:text-primary">
              read log
            </button>
            <button type="button" onClick={onRetry} className="flex-none border border-acc-line px-3 py-1 text-[12px] hover:border-primary hover:text-primary">
              retry
            </button>
          </>
        ) : task.prUrl ? (
          <a
            href={task.prUrl}
            target="_blank"
            rel="noreferrer"
            className="flex-none border border-acc-line px-3 py-1 text-[12px] hover:border-primary hover:text-primary"
          >
            review
          </a>
        ) : null}
      </div>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// The WORKING table: nothing here needs a human, so it's dense and calm. The
// live tool line is what makes "is it actually doing something" answerable
// without opening the session.
function WorkingRow({
  task,
  summary,
  last,
  onSelect,
  onDelete,
}: {
  task: Task;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = task.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(task.podPhase) && !provisioning;
  const stale = staleBadge(task);

  return (
    <div className={`relative ${last ? "" : "border-b border-line3"}`}>
      <div className="flex items-center gap-3 px-4 py-2.5 pr-8">
        <span
          className={`w-[7px] h-[7px] rounded-full flex-none ${
            stale ? "bg-error" : live ? "bg-info animate-fpulse" : "border border-dim2"
          }`}
        />
        <span className={`text-[12.5px] flex-none ${live ? "text-text2" : "text-dim2"}`}>#{task.id.slice(0, 6)}</span>
        <button
          type="button"
          onClick={onSelect}
          className={`text-[13px] text-left hover:text-primary cursor-pointer min-w-0 truncate ${live ? "" : "text-dim"}`}
        >
          {task.description}
        </button>
        <span className="text-[11.5px] text-dim2 flex-none">{repoLabel(task)}</span>
        {task.permissionMode === "bypassPermissions" && (
          <span
            className="text-[11px] text-warning border border-orange-line px-1.5 py-px flex-none"
            title="every tool call runs without asking"
          >
            bypass
          </span>
        )}

        {provisioning ? (
          <>
            <span className="ml-auto text-[12px] text-dim2 min-w-0 truncate">
              booting{task.podMessage ? ` · ${task.podMessage}` : ""}
            </span>
            <span className="w-[105px] h-[3px] bar-provisioning flex-none" />
            <span className="text-[11px] text-dim2 w-6 text-right flex-none">—</span>
          </>
        ) : (
          <>
            <span className="ml-auto text-[12px] text-dim min-w-0 truncate">
              {inFlight
                ? `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${
                    inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : ""
                  }`
                : stale
                  ? stale.label.toLowerCase()
                  : "idle"}
            </span>
            <TickBar todos={todos} cell="w-4" className="flex-none" />
            <span className="text-[11px] text-dim2 w-6 text-right flex-none">
              {todos.length > 0 ? todoProgress(todos) : "—"}
            </span>
          </>
        )}
      </div>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// The collapsed tail. A session in here needs nothing; it exists so the count is
// honest and the row is still reachable, not to be read.
function QuietGroup({
  title,
  tasks,
  onSelect,
}: {
  title: string;
  tasks: Task[];
  onSelect: (id: string) => void;
}) {
  if (tasks.length === 0) return null;
  return (
    <Collapse
      summary={<span className="text-[11.5px] text-dim2">▸ {title} · {tasks.length}</span>}
      summaryClassName="py-1"
      contentClassName="pl-3 py-1 flex flex-col gap-1"
    >
      {tasks.map((t) => {
        const badge = sessionBadge(t);
        return (
          <button
            key={t.id}
            type="button"
            onClick={() => onSelect(t.id)}
            className="flex items-center gap-2.5 text-left hover:text-primary cursor-pointer"
          >
            <span className="text-[11px] text-dim2 flex-none">#{t.id.slice(0, 6)}</span>
            <span className="text-[12px] text-dim min-w-0 truncate flex-1">{t.description}</span>
            {badge && (
              <span className={`text-[10px] px-1 border tracking-wide flex-none ${badge.className}`} title={badge.title}>
                {badge.label}
              </span>
            )}
          </button>
        );
      })}
    </Collapse>
  );
}

export function TaskList({
  tasks,
  summaries,
  needsYouIds,
  onSelect,
  onDelete,
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
  const { needsYou, working, finished, stalled, proposed, quiet } = bucketTasks(tasks, needsYouIds);

  if (tasks.length === 0) {
    return <div className="p-5 text-[13px] text-dim">No sessions.</div>;
  }

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-4.5 pt-5 pb-6 flex flex-col gap-3">
      {needsYou.length > 0 && (
        <>
          <SectionHeading title="NEEDS YOU" tone="pink" className="mb-0.5" />
          {needsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              task={t}
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
              task={t}
              onSelect={() => onSelect(t.id)}
              onRetry={() => onRetry(t.id)}
              onOpenLogs={() => onOpenLogs(t.id)}
              onDelete={() => onDelete(t.id)}
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
                task={t}
                summary={summaries.get(t.id)}
                last={i === working.length - 1}
                onSelect={() => onSelect(t.id)}
                onDelete={() => onDelete(t.id)}
              />
            ))}
          </div>
        </>
      )}

      <div className="flex flex-col gap-1 mt-3.5">
        <QuietGroup title="stalled" tasks={stalled} onSelect={onSelect} />
        <QuietGroup title="proposed by audits" tasks={proposed} onSelect={onSelect} />
        <QuietGroup title="idle" tasks={quiet} onSelect={onSelect} />
      </div>
    </div>
  );
}
