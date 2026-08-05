import { useCallback, useEffect, useMemo, useState } from "react";
import { TaskList } from "./pages/TaskList";
import { TaskDetail } from "./pages/TaskDetail";
import { Worktrees } from "./pages/Worktrees";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { client } from "./connectClient";
import type { Task } from "./gen/agentfleet/v1/dashboard_pb";
import { enrichTask } from "./mockEnrichment";

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
  const [view, setView] = useState<"tasks" | "worktrees">(readViewFromUrl);
  const [selectedId, setSelectedId] = useState<string | null>(
    readTaskIdFromUrl,
  );
  const [tasks, setTasks] = useState<Task[]>([]);
  const [tasksError, setTasksError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");

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

  function selectTask(id: string) {
    setSelectedId(id);
    setView("tasks");
    const url = new URL(window.location.href);
    url.searchParams.set("task", id);
    url.searchParams.delete("view");
    window.history.pushState({}, "", url);
  }

  function selectView(next: "tasks" | "worktrees") {
    setView(next);
    const url = new URL(window.location.href);
    if (next === "worktrees") url.searchParams.set("view", "worktrees");
    else url.searchParams.delete("view");
    window.history.pushState({}, "", url);
  }

  const needsYouCount = useMemo(
    () => tasks.filter((t) => enrichTask(t).needsYou).length,
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
    <div className="min-h-screen bg-base-100 flex flex-col">
      <div className="flex-none flex items-center gap-5 px-5 h-13 border-b border-base-content/10 bg-base-200">
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
          <span>{tasks.length} sessions live</span>
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

      {tasksError && (
        <div className="alert alert-error m-2 text-sm">{tasksError}</div>
      )}

      {view === "worktrees" ? (
        <Worktrees />
      ) : (
        <div className="flex flex-col lg:flex-row flex-1 min-h-0">
          <div className="lg:w-[320px] flex-none border-b lg:border-b-0 lg:border-r border-base-content/10 bg-base-200 overflow-y-auto">
            <TaskList tasks={filteredTasks} selectedId={selectedId} onSelect={selectTask} />
          </div>
          <div className="flex-1 min-w-0">
            {selectedId ? (
              <TaskDetail taskId={selectedId} tasks={tasks} onSelect={selectTask} />
            ) : (
              <div className="p-4 opacity-60">Select a task to view details.</div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
