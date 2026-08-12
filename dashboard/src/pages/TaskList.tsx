import type { Task } from "../gen/agentfleet/v1/core_pb";
import { repoLabel } from "../taskKind";
import type { TodoItem } from "../transcript";

export const ACTIVE_STATUSES = new Set(["pending", "claimed", "running"]);
export const SHIPPED_STATUSES = new Set(["done", "failed", "cancelled"]);

// Shared section-membership split — used by both the desktop TaskList and
// MobileTaskList so the two views never disagree about which bucket a task
// lands in.
export function bucketTasks(tasks: Task[], needsYouIds: Set<string>) {
  return {
    // A 'proposed' task is by definition a decision waiting on a human
    // (Alertmanager or the audit scheduler created it and nobody has
    // approved it), so it belongs here regardless of needsYouIds — that
    // set tracks outstanding transcript questions, and a proposal has no
    // transcript yet.
    //
    // Deliberately NOT added to ACTIVE_STATUSES: App.tsx uses that same
    // set for the per-task todo fetch and the "sessions live" counter,
    // and a proposal is neither. Without this line a proposal matches
    // neither hardcoded set and vanishes from both the desktop and mobile
    // lists — the one place the human is supposed to see it.
    needsYou: tasks.filter((t) => t.status === "proposed" || (ACTIVE_STATUSES.has(t.status) && needsYouIds.has(t.id))),
    working: tasks.filter((t) => ACTIVE_STATUSES.has(t.status) && !needsYouIds.has(t.id)),
    shipped: tasks.filter((t) => SHIPPED_STATUSES.has(t.status)),
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

// Mirrors core/internal/tasks/store.go's IsPodPhaseLive — the client-side
// half of the same "does this session have a live pod right now" check
// the Warm/Discuss server handlers use, so ActionsMenu can show Warm vs.
// Stop without a round trip (sessions redesign, supersedes docs/adr/0021/
// 0025's phase-boundary framing).
export function isPodPhaseLive(phase?: string): boolean {
  return phase === "POD_PHASE_PROVISIONING" || phase === "POD_PHASE_CREATED" || phase === "POD_PHASE_SCHEDULED" || phase === "POD_PHASE_RUNNING";
}


// Matches core's own reclaim threshold (tasks.Store.ClaimNextTask reclaims
// a claimed/running task once its heartbeat is this stale) —
// the dashboard's "stuck" signal should fire at exactly the point a human
// can no longer distinguish "about to be reclaimed" from "silently wedged
// forever" (see the incident where a task sat claimed with no pod for 20+
// minutes and nothing in the UI showed it).
const STALE_THRESHOLD_MS = 10 * 60 * 1000;

// ONE badge per session, chosen by precedence.
//
// Status, pod phase, liveness and staleness were four independently
// rendered badges of near-identical weight, and they overlap hard: a
// `claimed`/`running` status means "there is a live pod", which is what the
// pod badge already says; `unknown` liveness renders "STARTING", which is
// what `PROVISIONING`/`SCHEDULED` already says. Four badges that mostly
// restate each other read as noise, and the one that matters — a session
// waiting on a human — does not stand out from the three that don't.
//
// The order below is "what does a human need to know first", not the order
// the data happens to arrive in. Everything demoted out of the label
// survives in the tooltip, so nothing is lost, only ranked.
export function sessionBadge(task: Task): { label: string; className: string; title?: string } | null {
  const stale = staleBadge(task);
  const phase = task.podPhase?.replace("POD_PHASE_", "");

  // 1. Needs a human. Always wins: nothing else about this session matters
  //    until someone clicks, and this is the product's whole job.
  if (task.liveState === "blocked") {
    return { label: "NEEDS YOU", className: "text-primary border-primary/45 bg-primary/10", title: "waiting on your decision" };
  }
  // 2. Something is wrong, in order of how wrong.
  if (phase === "CRASHED") {
    return { label: "CRASHED", className: "text-error border-error/45 bg-error/10", title: task.podMessage || task.lastError || undefined };
  }
  if (stale) return stale;
  if (task.liveState === "stalled") {
    return { label: "STALLED", className: "text-warning border-warning/45 bg-warning/10", title: "no response since the last thing sent to the agent" };
  }
  // 3. Finished while nobody was looking — the "welcome back" state.
  if (task.liveState === "done") {
    return { label: "DONE", className: "text-success border-success/45 bg-success/10", title: "finished while you weren't looking — opening it marks it seen" };
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
    return { label: "IDLE", className: "text-base-content/50 border-base-content/20 bg-base-content/5", title: "pod live, nothing in flight" };
  }
  // 5. No live pod: the workflow status is the only thing left to say, and
  //    now it has the badge to itself instead of competing with three
  //    others.
  if (task.status === "proposed") {
    return { label: "PROPOSED", className: "text-warning border-warning/45 bg-warning/10", title: "machine-created — approve it to dispatch" };
  }
  if (task.status === "pending") {
    return { label: "QUEUED", className: "text-base-content/60 border-base-content/20 bg-base-content/5" };
  }
  if (task.status === "failed" || task.status === "failed_permanently") {
    return { label: task.status === "failed" ? "FAILED" : "FAILED (final)", className: "text-error border-error/45 bg-error/10", title: task.lastError || undefined };
  }
  if (task.status === "cancelled") {
    return { label: "CANCELLED", className: "text-warning border-warning/45 bg-warning/10" };
  }
  if (task.status === "done") {
    return { label: "DONE", className: "text-success border-success/45 bg-success/10" };
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
    className: "text-error border-error/45 bg-error/10 animate-pulse",
    title: task.lastError || `no heartbeat for ${minutes}m — will be reclaimed and redispatched`,
  };
}

export function heartbeatLabel(task: Task): string | null {
  if (!task.heartbeatAt) return null;
  const minutes = Math.floor((Date.now() - new Date(task.heartbeatAt).getTime()) / 60_000);
  return minutes < 1 ? "heartbeat just now" : `heartbeat ${minutes}m ago`;
}

function SidebarSection({
  title,
  count,
  children,
}: {
  title: string;
  count: number;
  children: React.ReactNode;
}) {
  if (count === 0) return null;
  return (
    <div>
      <div className="px-4 pt-4 pb-2 flex items-baseline gap-2">
        <span className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">
          {title}
        </span>
        <span className="text-[9.5px] text-base-content/35">{count}</span>
      </div>
      {children}
    </div>
  );
}

// A small always-visible delete control, absolutely positioned over its
// sibling select-button — kept a sibling rather than nested inside that
// button (buttons can't nest), the browser routes the click to whichever
// one is topmost at that point so no stopPropagation is needed either.
function DeleteButton({ onDelete }: { onDelete: () => void }) {
  return (
    <button
      type="button"
      onClick={onDelete}
      title="Delete this session"
      className="absolute top-2 right-2 w-5 h-5 flex items-center justify-center rounded text-base-content/30 hover:text-error hover:bg-error/10 text-[11px]"
    >
      ✕
    </button>
  );
}

function NeedsYouCard({
  task,
  todos,
  selected,
  onSelect,
  onDelete,
}: {
  task: Task;
  todos: TodoItem[];
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const heartbeat = heartbeatLabel(task);
  const progress = todos.filter((t) => t.status === "completed").length;
  const badge = sessionBadge(task);
  return (
    <div className="relative">
      <button
        type="button"
        onClick={onSelect}
        className={`w-full text-left px-4 py-3 pr-8 border-l-2 border-b border-base-content/5 ${
          selected ? "bg-base-300" : "bg-base-300/40 hover:bg-base-300/70"
        }`}
        style={{ borderLeftColor: "var(--color-primary)" }}
      >
        <div className="flex items-center gap-2">
          <span className="text-[11px] font-semibold">#{task.id.slice(0, 6)}</span>
          <span className="text-[10px] text-base-content/50">{repoLabel(task)}</span>
          {badge && (
            <span
              className={`text-[8.5px] px-1 rounded border tracking-wide whitespace-nowrap overflow-hidden text-ellipsis max-w-[140px] flex-none ${badge.className}`}
              title={badge.title ?? badge.label}
            >
              {badge.label}
            </span>
          )}
          {heartbeat && <span className="ml-auto text-[9.5px] text-primary">{heartbeat}</span>}
        </div>
        <div className="text-[11.5px] leading-relaxed mt-1.5 text-base-content/90">
          {task.description}
        </div>
        <div className="text-[9.5px] text-base-content/40 mt-1.5">
          {progress} of {todos.length} todos
        </div>
      </button>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

function WorkingCard({
  task,
  todos,
  selected,
  onSelect,
  onDelete,
}: {
  task: Task;
  todos: TodoItem[];
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const progress = todos.filter((t) => t.status === "completed").length;
  const pct = Math.round((progress / Math.max(todos.length, 1)) * 100);
  const badge = sessionBadge(task);
  return (
    <div className="relative">
      <button
        type="button"
        onClick={onSelect}
        className={`w-full text-left px-4 py-2.5 pr-8 border-b border-base-content/5 ${
          selected ? "bg-base-300" : "hover:bg-base-300/50"
        }`}
      >
        <div className="flex items-center gap-2">
          <span className={`w-1.5 h-1.5 rounded-full ${staleBadge(task) ? "bg-error" : "bg-info animate-fpulse"}`} />
          <span className="text-[11px]">#{task.id.slice(0, 6)}</span>
          <span className="text-[10px] text-base-content/50">{repoLabel(task)}</span>
          {badge && (
            <span
              className={`text-[8.5px] px-1 rounded border tracking-wide whitespace-nowrap overflow-hidden text-ellipsis max-w-[140px] flex-none ${badge.className}`}
              title={badge.title ?? badge.label}
            >
              {badge.label}
            </span>
          )}
        </div>
        <div className="text-[11px] leading-relaxed mt-1 text-base-content/70 truncate">
          {task.description}
        </div>
        <div className="flex items-center gap-2 mt-1.5">
          <div className="flex-1 h-[3px] bg-base-content/10 rounded-full">
            <div className="h-[3px] bg-info rounded-full" style={{ width: `${pct}%` }} />
          </div>
          <span className="text-[9.5px] text-base-content/40">
            {progress}/{todos.length}
          </span>
        </div>
      </button>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

function ShippedRow({
  task,
  selected,
  onSelect,
  onDelete,
}: {
  task: Task;
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const badge = prBadge(task);
  return (
    <div className="relative">
      <button
        type="button"
        onClick={onSelect}
        className={`w-full text-left px-4 py-2 pr-8 border-b border-base-content/5 flex items-center gap-2 ${
          selected ? "bg-base-300" : "hover:bg-base-300/50"
        }`}
      >
        <span className="text-[10.5px] text-base-content/50">#{task.id.slice(0, 6)}</span>
        <span className="text-[10.5px] text-base-content/70 flex-1 truncate">{task.description}</span>
        {badge && <span className={`text-[9.5px] ${badge.className}`}>{badge.label}</span>}
      </button>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

export function TaskList({
  tasks,
  selectedId,
  needsYouIds,
  todosById,
  onSelect,
  onDelete,
}: {
  tasks: Task[];
  selectedId: string | null;
  needsYouIds: Set<string>;
  todosById: Map<string, TodoItem[]>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
}) {
  const { needsYou, working, shipped } = bucketTasks(tasks, needsYouIds);

  if (tasks.length === 0) {
    return <div className="p-4 text-sm opacity-60">No tasks.</div>;
  }

  return (
    <div className="pb-3">
      <SidebarSection title="NEEDS YOU" count={needsYou.length}>
        {needsYou.map((t) => (
          <NeedsYouCard
            key={t.id}
            task={t}
            todos={todosById.get(t.id) ?? []}
            selected={t.id === selectedId}
            onSelect={() => onSelect(t.id)}
            onDelete={() => onDelete(t.id)}
          />
        ))}
      </SidebarSection>
      <SidebarSection title="WORKING" count={working.length}>
        {working.map((t) => (
          <WorkingCard
            key={t.id}
            task={t}
            todos={todosById.get(t.id) ?? []}
            selected={t.id === selectedId}
            onSelect={() => onSelect(t.id)}
            onDelete={() => onDelete(t.id)}
          />
        ))}
      </SidebarSection>
      <SidebarSection title="SHIPPED" count={shipped.length}>
        {shipped.map((t) => (
          <ShippedRow
            key={t.id}
            task={t}
            selected={t.id === selectedId}
            onSelect={() => onSelect(t.id)}
            onDelete={() => onDelete(t.id)}
          />
        ))}
      </SidebarSection>
    </div>
  );
}
