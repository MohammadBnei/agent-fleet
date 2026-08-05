import type { Task } from "../gen/agentfleet/v1/dashboard_pb";
import { enrichTask } from "../mockEnrichment";

export const ACTIVE_STATUSES = new Set(["pending", "claimed", "planning", "implementing"]);
export const SHIPPED_STATUSES = new Set(["done", "failed", "cancelled"]);

// Shared section-membership split — used by both the desktop TaskList and
// MobileTaskList so the two views never disagree about which bucket a task
// lands in.
export function bucketTasks(tasks: Task[], needsYouIds: Set<string>) {
  return {
    needsYou: tasks.filter((t) => ACTIVE_STATUSES.has(t.status) && needsYouIds.has(t.id)),
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
  selected,
  onSelect,
  onDelete,
}: {
  task: Task;
  selected: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const enrichment = enrichTask(task);
  const progress = enrichment.todos.filter((t) => t.done).length;
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
          <span className="text-[10px] text-base-content/50">{task.repo}</span>
          <span className="ml-auto text-[9.5px] text-primary">{enrichment.idleLabel}</span>
        </div>
        <div className="text-[11.5px] leading-relaxed mt-1.5 text-base-content/90">
          {task.description}
        </div>
        <div className="text-[9.5px] text-base-content/40 mt-1.5">
          {progress} of {enrichment.todos.length} todos
        </div>
      </button>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

function WorkingCard({
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
  const enrichment = enrichTask(task);
  const progress = enrichment.todos.filter((t) => t.done).length;
  const pct = Math.round((progress / Math.max(enrichment.todos.length, 1)) * 100);
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
          <span className="w-1.5 h-1.5 rounded-full bg-info animate-fpulse" />
          <span className="text-[11px]">#{task.id.slice(0, 6)}</span>
          <span className="text-[10px] text-base-content/50">{task.repo}</span>
        </div>
        <div className="text-[11px] leading-relaxed mt-1 text-base-content/70 truncate">
          {task.description}
        </div>
        <div className="flex items-center gap-2 mt-1.5">
          <div className="flex-1 h-[3px] bg-base-content/10 rounded-full">
            <div className="h-[3px] bg-info rounded-full" style={{ width: `${pct}%` }} />
          </div>
          <span className="text-[9.5px] text-base-content/40">
            {progress}/{enrichment.todos.length}
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
  onSelect,
  onDelete,
}: {
  tasks: Task[];
  selectedId: string | null;
  needsYouIds: Set<string>;
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
