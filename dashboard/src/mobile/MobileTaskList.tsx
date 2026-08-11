import type { Task } from "../gen/agentfleet/v1/core_pb";
import { repoLabel } from "../taskKind";
import { bucketTasks, prBadge, heartbeatLabel } from "../pages/TaskList";
import { NewTaskDialog } from "../components/NewTaskDialog";
import type { TodoItem } from "../transcript";

// Mirrors the "herd" mock's phone list screen (Agent Fleet Mobile.dc.html)
// minus the device chrome (status bar / home indicator are the design
// tool's own preview frame, not real UI). Pure presentation — all data
// comes from App.tsx, same as the desktop TaskList.

function NeedsYouCard({ task, todos, onSelect }: { task: Task; todos: TodoItem[]; onSelect: () => void }) {
  const heartbeat = heartbeatLabel(task);
  const done = todos.filter((t) => t.status === "completed").length;
  return (
    <button
      type="button"
      onClick={onSelect}
      className="block text-left mx-3.5 mb-2.5 p-3.5 rounded-xl bg-base-300 border border-primary/35 w-auto"
      style={{ borderLeftWidth: 3, borderLeftColor: "var(--color-primary)" }}
    >
      <div className="flex items-center gap-2 min-w-0 flex-wrap">
        <span className="text-[12px] font-semibold flex-none">#{task.id.slice(0, 6)}</span>
        <span className="text-[10.5px] text-base-content/50 flex-1 min-w-0 break-words">{repoLabel(task)}</span>
        {heartbeat && <span className="text-[10px] text-primary flex-none whitespace-nowrap">{heartbeat}</span>}
      </div>
      <div className="text-[13px] leading-relaxed mt-2 text-base-content/95 break-words">{task.description}</div>
      <div className="flex gap-1 mt-3">
        {todos.map((t, i) => (
          <div key={i} className={`flex-1 h-[5px] rounded ${t.status === "completed" ? "bg-secondary" : "bg-base-content/10"}`} />
        ))}
      </div>
      <div className="flex items-center gap-2 mt-1.5">
        <span className="text-[10px] text-base-content/50">
          {done} of {todos.length} todos
        </span>
        <span className="ml-auto px-3.5 py-1.5 rounded-md bg-primary text-primary-content text-[11px] font-semibold min-h-8 flex items-center whitespace-nowrap">
          answer
        </span>
      </div>
    </button>
  );
}

function WorkingCard({ task, todos, onSelect }: { task: Task; todos: TodoItem[]; onSelect: () => void }) {
  const heartbeat = heartbeatLabel(task);
  const done = todos.filter((t) => t.status === "completed").length;
  const pct = Math.round((done / Math.max(todos.length, 1)) * 100);
  return (
    <button
      type="button"
      onClick={onSelect}
      className="block text-left mx-3.5 mb-2.5 p-3.5 rounded-xl border border-base-content/10 w-auto"
    >
      <div className="flex items-center gap-2 min-w-0 flex-wrap">
        <span className="w-1.5 h-1.5 rounded-full bg-secondary animate-fpulse flex-none" />
        <span className="text-[12px] flex-none">#{task.id.slice(0, 6)}</span>
        <span className="text-[10.5px] text-base-content/50 flex-1 min-w-0 break-words">{repoLabel(task)}</span>
        {heartbeat && <span className="text-[10px] text-base-content/50 flex-none whitespace-nowrap">{heartbeat}</span>}
      </div>
      <div className="text-[12.5px] leading-relaxed mt-2 text-base-content/85 break-words">{task.description}</div>
      <div className="flex items-center gap-2 mt-2">
        <div className="flex-1 h-1 bg-base-content/10 rounded-full">
          <div className="h-1 bg-secondary rounded-full" style={{ width: `${pct}%` }} />
        </div>
        <span className="text-[10px] text-base-content/50 whitespace-nowrap">
          {done}/{todos.length}
        </span>
      </div>
    </button>
  );
}

function ShippedCard({ task, onSelect }: { task: Task; onSelect: () => void }) {
  const badge = prBadge(task);
  return (
    <button
      type="button"
      onClick={onSelect}
      className="flex items-center gap-2.5 text-left mx-3.5 mb-2 px-3.5 py-2.5 rounded-lg border border-base-content/5 w-auto"
    >
      <span className="text-[10.5px] text-base-content/50 flex-none whitespace-nowrap">#{task.id.slice(0, 6)}</span>
      <span className="text-[11.5px] text-base-content/70 flex-1 min-w-0 break-words">{task.description}</span>
      {badge && <span className={`text-[10px] ${badge.className} flex-none whitespace-nowrap`}>{badge.label}</span>}
    </button>
  );
}

function Section({ title, count, children }: { title: string; count: number; children: React.ReactNode }) {
  if (count === 0) return null;
  return (
    <div className="pt-3.5">
      <div className="px-4 pb-2 flex items-baseline gap-2">
        <span className="text-[9.5px] tracking-[0.11em] font-semibold text-base-content/60">{title}</span>
        <span className="text-[9.5px] text-base-content/35">{count}</span>
      </div>
      {children}
    </div>
  );
}

export function MobileTaskList({
  tasks,
  filteredTasks,
  needsYouIds,
  todosById,
  needsYouCount,
  liveCount,
  repoCount,
  filter,
  setFilter,
  onSelect,
  onOpenWorktrees,
  onOpenFiles,
  onCreated,
}: {
  tasks: Task[];
  filteredTasks: Task[];
  needsYouIds: Set<string>;
  todosById: Map<string, TodoItem[]>;
  needsYouCount: number;
  liveCount: number;
  repoCount: number;
  filter: string;
  setFilter: (v: string) => void;
  onSelect: (id: string) => void;
  onOpenWorktrees: () => void;
  onOpenFiles: () => void;
  onCreated: (id: string) => void;
}) {
  const { needsYou, working, shipped } = bucketTasks(filteredTasks, needsYouIds);

  return (
    <div className="flex flex-col h-full bg-base-100">
      <div className="flex-none px-4 pt-3.5 pb-3 border-b border-base-content/10 bg-base-200">
        <div className="flex items-center gap-2.5 mb-3">
          <span className="font-display font-semibold text-[22px]">herd</span>
          <span className="text-[10px] text-base-content/50 whitespace-nowrap">
            {liveCount} sessions · {repoCount} repos
          </span>
          <button
            type="button"
            onClick={onOpenFiles}
            className="ml-auto text-[10px] text-base-content/50 px-2 py-1 rounded-md border border-base-content/10 whitespace-nowrap"
          >
            Files
          </button>
          <button
            type="button"
            onClick={onOpenWorktrees}
            className="text-[10px] text-base-content/50 px-2 py-1 rounded-md border border-base-content/10 whitespace-nowrap"
          >
            Worktrees
          </button>
        </div>
        <div className="mb-3">
          <NewTaskDialog onCreated={onCreated} />
        </div>
        <div className="flex items-center gap-2.5">
          <div className="flex-1 flex items-center gap-2 px-3 py-2 border border-base-content/10 rounded-lg text-[11px] text-base-content/50 min-w-0">
            <span>⌕</span>
            <input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="filter sessions"
              className="bg-transparent outline-none flex-1 text-base-content placeholder:text-base-content/40 min-w-0"
            />
          </div>
        </div>
        {needsYouCount > 0 && (
          <div className="flex items-center gap-2.5 mt-3 px-3 py-2.5 border border-primary/45 rounded-lg bg-primary/10">
            <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse flex-none" />
            <span className="text-[12px] text-primary font-semibold flex-1 min-w-0">{needsYouCount} agents waiting on you</span>
            <span className="text-[11px] text-primary flex-none">→</span>
          </div>
        )}
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto pb-3">
        {tasks.length === 0 ? (
          <div className="p-4 text-sm opacity-60">No tasks.</div>
        ) : (
          <>
            <Section title="NEEDS YOU" count={needsYou.length}>
              {needsYou.map((t) => (
                <NeedsYouCard key={t.id} task={t} todos={todosById.get(t.id) ?? []} onSelect={() => onSelect(t.id)} />
              ))}
            </Section>
            <Section title="WORKING" count={working.length}>
              {working.map((t) => (
                <WorkingCard key={t.id} task={t} todos={todosById.get(t.id) ?? []} onSelect={() => onSelect(t.id)} />
              ))}
            </Section>
            <Section title="SHIPPED" count={shipped.length}>
              {shipped.map((t) => (
                <ShippedCard key={t.id} task={t} onSelect={() => onSelect(t.id)} />
              ))}
            </Section>
          </>
        )}
      </div>
    </div>
  );
}
