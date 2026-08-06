import { useCallback, useEffect, useMemo, useState } from "react";
import { TaskList, ACTIVE_STATUSES } from "./pages/TaskList";
import { TaskDetail } from "./pages/TaskDetail";
import { Worktrees } from "./pages/Worktrees";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { ManageReposModal } from "./components/ManageReposModal";
import { MobileTaskList } from "./mobile/MobileTaskList";
import { MobileTaskDetail } from "./mobile/MobileTaskDetail";
import { client } from "./connectClient";
import type { Task } from "./gen/agentfleet/v1/dashboard_pb";
import { findPendingQuestion, latestTodos, type TodoItem } from "./transcript";
import { ErrorModal } from "./components/ErrorModal";
import { ConfirmModal } from "./components/ConfirmModal";
import { useMediaQuery } from "./useMediaQuery";

// No router library for two views (see docs/adr/0013's plan) — state
// mirrored to ?task=<id> so a task's detail view is still bookmarkable/
// shareable without pulling in react-router for this little surface.
function readTaskIdFromUrl(): string | null {
  return new URLSearchParams(window.location.search).get("task");
}

// Worktrees (reliability-findings.md #2's manual cleanup view) is a third
// top-level view, same no-router/URL-param pattern as the task list/detail
// split above.
function readViewFromUrl(): "tasks" | "worktrees" {
  return new URLSearchParams(window.location.search).get("view") === "worktrees" ? "worktrees" : "tasks";
}

// Plain polling, not a stream — a second live feed just for the list is
// unjustified scope for v1 (see docs/adr/0014); the detail view's
// StreamTranscript RPC is where "live" actually matters. Lives here (not
// inside TaskList) so TaskDetail's "rest of the herd" strip can share the
// same fetched list instead of each view polling independently.
const POLL_INTERVAL_MS = 5000;

export default function App() {
  // Matches Tailwind's `sm:` breakpoint. Desktop (TaskDetail) and mobile
  // (MobileTaskDetail) used to both mount unconditionally, just CSS-hidden
  // via `hidden sm:*`/`sm:hidden` — each ran its own useTaskDetail, meaning
  // two independent StreamTranscript subscriptions polling the same task
  // concurrently whenever a task was open. Gating the mount itself on the
  // actual viewport instead of hiding one with CSS stops that duplicate
  // background streaming outright.
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [view, setView] = useState<"tasks" | "worktrees">(readViewFromUrl);
  const [selectedId, setSelectedId] = useState<string | null>(
    readTaskIdFromUrl,
  );
  const [tasks, setTasks] = useState<Task[]>([]);
  const [tasksError, setTasksError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [needsYouIds, setNeedsYouIds] = useState<Set<string>>(new Set());
  const [todosById, setTodosById] = useState<Map<string, TodoItem[]>>(new Map());
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);

  const loadTasks = useCallback(() => {
    return client
      .listTasks({})
      .then((res) => setTasks(res.tasks))
      .catch((err: Error) => setTasksError(err.message));
  }, []);

  useEffect(() => {
    loadTasks();
    const interval = setInterval(loadTasks, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [loadTasks]);

  // "Needs you" has no backend flag (yet) — derived here from the same
  // transcript state TaskDetail.tsx already uses to decide whether to show
  // a QuestionCard: the latest QUESTION entry with no later ANSWER. Scoped
  // to ACTIVE_STATUSES tasks only, bounded by the fleet's default
  // concurrency cap of 5 (see CLAUDE.md), on the same 5s poll cadence as
  // loadTasks — an extra live stream per active task just for this would
  // contradict this file's own "plain polling, not a stream" call above.
  // Same fetch also derives each task's real todos (latest TodoWrite call)
  // for the list cards' progress bars — one more field off data already in
  // hand, not a second round of per-task requests.
  useEffect(() => {
    const active = tasks.filter((t) => ACTIVE_STATUSES.has(t.status));
    if (active.length === 0) {
      setNeedsYouIds(new Set());
      setTodosById(new Map());
      return;
    }
    let cancelled = false;
    Promise.all(
      active.map((t) =>
        client
          .getTranscript({ taskId: t.id, sinceSeq: 0n })
          .then((res) => ({ id: t.id, pending: findPendingQuestion(res.entries) !== null, todos: latestTodos(res.entries) }))
          .catch(() => ({ id: t.id, pending: false, todos: null as TodoItem[] | null })),
      ),
    ).then((results) => {
      if (cancelled) return;
      setNeedsYouIds(new Set(results.filter((r) => r.pending).map((r) => r.id)));
      setTodosById(new Map(results.filter((r): r is typeof r & { todos: TodoItem[] } => r.todos !== null).map((r) => [r.id, r.todos])));
    });
    return () => {
      cancelled = true;
    };
  }, [tasks]);

  function selectTask(id: string) {
    setSelectedId(id);
    setView("tasks");
    const url = new URL(window.location.href);
    url.searchParams.set("task", id);
    url.searchParams.delete("view");
    window.history.pushState({}, "", url);
  }

  // Only real caller today is the mobile back button — desktop's split-pane
  // layout has no "go back", it just leaves the "select a task" placeholder
  // showing when nothing's selected.
  function clearSelection() {
    setSelectedId(null);
    const url = new URL(window.location.href);
    url.searchParams.delete("task");
    window.history.pushState({}, "", url);
  }

  // Force-tears-down any live session then soft-deletes the task (see
  // docs/adr for DashboardService.DeleteTask) — used by both the desktop
  // TaskList's per-row delete button and the mobile session screen's "⋯".
  // Confirmation is a ConfirmModal, not window.confirm (native dialogs
  // can't be themed and block the render thread) — deleteTask just opens
  // it; confirmDeleteTask does the actual call.
  function deleteTask(id: string) {
    setPendingDeleteId(id);
  }

  function confirmDeleteTask() {
    const id = pendingDeleteId;
    setPendingDeleteId(null);
    if (!id) return;
    client
      .deleteTask({ taskId: id })
      .then(() => {
        loadTasks();
        if (id === selectedId) clearSelection();
      })
      .catch((err: Error) => setTasksError(err.message));
  }

  function selectView(next: "tasks" | "worktrees") {
    setView(next);
    const url = new URL(window.location.href);
    if (next === "worktrees") url.searchParams.set("view", "worktrees");
    else url.searchParams.delete("view");
    window.history.pushState({}, "", url);
  }

  const needsYouCount = useMemo(
    () => tasks.filter((t) => needsYouIds.has(t.id)).length,
    [tasks, needsYouIds],
  );
  // "sessions live" means running, not "ever returned by listTasks" — the
  // backend keeps returning done/failed/cancelled tasks (list history), so
  // tasks.length alone never decrements.
  const liveCount = useMemo(
    () => tasks.filter((t) => ACTIVE_STATUSES.has(t.status)).length,
    [tasks],
  );
  const repoCount = useMemo(
    () => new Set(tasks.map((t) => t.repo)).size,
    [tasks],
  );
  const filteredTasks = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return tasks;
    return tasks.filter(
      (t) =>
        t.repo.toLowerCase().includes(q) ||
        t.description.toLowerCase().includes(q),
    );
  }, [tasks, filter]);

  return (
    // h-dvh (dynamic viewport height), not h-screen (100vh) — 100vh is
    // pinned to the largest possible viewport on mobile browsers, so it
    // doesn't shrink when the on-screen keyboard opens; the composer at the
    // bottom of the flex column ends up rendered underneath the keyboard
    // instead of above it. h-dvh tracks the actual visible viewport.
    <div className="h-dvh overflow-hidden bg-base-100 grid grid-rows-[auto_1fr]">
      {/* Desktop chrome — the mobile view (below) has its own header inside
          MobileTaskList/MobileTaskDetail, matching the "herd" mock's phone
          screens, which don't share this row at all. */}
      {isDesktop && (
      <div className="flex row-start-1 items-center gap-5 px-5 h-13 border-b border-base-content/10 bg-base-200">
        <div className="flex items-baseline gap-2">
          <span className="font-display font-semibold text-base">herd</span>
          <span className="text-[10.5px] text-base-content/50">
            agent-fleet · ukubi-cluster
          </span>
        </div>

        {needsYouCount > 0 && (
          <div className="flex items-center gap-2 px-2.5 py-1 rounded-md border border-primary/45 bg-primary/10">
            <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />
            <span className="text-[10.5px] text-primary font-medium">
              {needsYouCount} waiting on you
            </span>
          </div>
        )}

        <div className="flex items-center gap-4 text-[10.5px] text-base-content/50">
          <NewTaskDialog
            onCreated={(id) => {
              loadTasks();
              selectTask(id);
            }}
          />
          <ManageReposModal />
          <span>{liveCount} sessions live</span>
          <span>{repoCount} repos</span>
        </div>

        <div className="flex items-center gap-1 text-[10.5px]">
          <button
            type="button"
            onClick={() => selectView("tasks")}
            className={`px-2.5 py-1 rounded-md ${view === "tasks" ? "bg-base-content/10 text-base-content" : "text-base-content/50 hover:text-base-content"}`}
          >
            Tasks
          </button>
          <button
            type="button"
            onClick={() => selectView("worktrees")}
            className={`px-2.5 py-1 rounded-md ${view === "worktrees" ? "bg-base-content/10 text-base-content" : "text-base-content/50 hover:text-base-content"}`}
          >
            Worktrees
          </button>
        </div>

        {view === "tasks" && (
          <div className="ml-auto flex items-center gap-2.5">
            <div className="flex items-center gap-2 px-2.5 py-1.5 border border-base-content/10 rounded-md text-[10.5px] text-base-content/50 w-48 focus-within:border-base-content/25">
              <span>⌕</span>
              <input
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="filter sessions"
                className="bg-transparent outline-none flex-1 text-base-content placeholder:text-base-content/40"
              />
            </div>
          </div>
        )}
      </div>
      )}

      <ErrorModal message={tasksError} onClose={() => setTasksError(null)} />
      <ConfirmModal
        open={pendingDeleteId !== null}
        message="Delete this session? This tears down any live pod and removes it from the list."
        onConfirm={confirmDeleteTask}
        onCancel={() => setPendingDeleteId(null)}
      />

      {isDesktop ? (
      <div className="grid grid-cols-1 lg:grid-cols-[320px_1fr] row-start-2 min-h-0">
        {view === "worktrees" ? (
          <Worktrees />
        ) : (
          <>
            <div className="border-b lg:border-b-0 lg:border-r border-base-content/10 bg-base-200 overflow-y-auto min-h-0">
              <TaskList
                tasks={filteredTasks}
                selectedId={selectedId}
                needsYouIds={needsYouIds}
                todosById={todosById}
                onSelect={selectTask}
                onDelete={deleteTask}
              />
            </div>
            <div className="min-w-0 min-h-0">
              {selectedId ? (
                <TaskDetail taskId={selectedId} tasks={tasks} onSelect={selectTask} />
              ) : (
                <div className="h-full flex flex-col items-center justify-center gap-2 text-base-content/30">
                  <span className="text-4xl">⌕</span>
                  <span className="text-[12px]">Select a task to view details</span>
                </div>
              )}
            </div>
          </>
        )}
      </div>
      ) : (
      <div className="row-start-2 min-h-0 flex flex-col">
        {view === "worktrees" ? (
          <Worktrees onBack={() => selectView("tasks")} />
        ) : selectedId ? (
          <MobileTaskDetail taskId={selectedId} onBack={clearSelection} onDelete={() => deleteTask(selectedId)} />
        ) : (
          <MobileTaskList
            tasks={tasks}
            filteredTasks={filteredTasks}
            needsYouIds={needsYouIds}
            todosById={todosById}
            needsYouCount={needsYouCount}
            liveCount={liveCount}
            repoCount={repoCount}
            filter={filter}
            setFilter={setFilter}
            onSelect={selectTask}
            onOpenWorktrees={() => selectView("worktrees")}
            onCreated={(id) => {
              loadTasks();
              selectTask(id);
            }}
          />
        )}
      </div>
      )}
    </div>
  );
}
