import { useCallback, useEffect, useMemo, useState } from "react";
import { TaskList, ACTIVE_STATUSES } from "./pages/TaskList";
import { TaskDetail } from "./pages/TaskDetail";
import { Worktrees } from "./pages/Worktrees";
import { Files } from "./pages/Files";
import { Audits } from "./pages/Audits";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { SettingsMenu } from "./components/SettingsMenu";
import { Segmented } from "./components/Segmented";
import { MobileTaskList } from "./mobile/MobileTaskList";
import { MobileTaskDetail } from "./mobile/MobileTaskDetail";
import { client } from "./connectClient";
import type { Task } from "./gen/agentfleet/v1/core_pb";
import { listSummary, type ListSummary } from "./transcript";
import { ErrorModal } from "./components/ErrorModal";
import { ConfirmModal } from "./components/ConfirmModal";
import { LogDrawer } from "./components/LogDrawer";
import { useMediaQuery } from "./useMediaQuery";
import { useTheme } from "./useTheme";

// No router library (see docs/adr/0013's plan) — state mirrored to
// ?view=/?task= so a session is still bookmarkable/shareable without pulling
// in react-router for this surface.
function readTaskIdFromUrl(): string | null {
  return new URLSearchParams(window.location.search).get("task");
}

export type View = "tasks" | "audits" | "worktrees" | "files";

function readViewFromUrl(): View {
  const v = new URLSearchParams(window.location.search).get("view");
  return v === "worktrees" || v === "files" || v === "audits" ? v : "tasks";
}

const NAV: readonly { value: View; label: string }[] = [
  { value: "tasks", label: "tasks" },
  { value: "audits", label: "audits" },
  { value: "worktrees", label: "worktrees" },
  { value: "files", label: "files" },
];

// Mobile's bottom bar has less room; "trees" is the console mockup's own label.
const MOBILE_NAV: readonly { value: View; label: string }[] = [
  { value: "tasks", label: "tasks" },
  { value: "audits", label: "audits" },
  { value: "worktrees", label: "trees" },
  { value: "files", label: "files" },
];

// Plain polling, not a stream — a second live feed just for the list is
// unjustified scope (docs/adr/0014); the detail view's StreamTranscript RPC is
// where "live" actually matters. Lives here rather than inside TaskList so the
// detail view can share the same fetched list instead of polling again.
const POLL_INTERVAL_MS = 5000;

export default function App() {
  // Matches Tailwind's `sm:`. Desktop and mobile detail views used to both
  // mount, CSS-hidden — each ran its own useTaskDetail, so two independent
  // StreamTranscript subscriptions polled the same task concurrently whenever
  // one was open. Gating the mount on the real viewport is what stops that.
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [theme, setTheme] = useTheme();
  const [view, setView] = useState<View>(readViewFromUrl);
  const [selectedId, setSelectedId] = useState<string | null>(readTaskIdFromUrl);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [tasksError, setTasksError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [summaries, setSummaries] = useState<Map<string, ListSummary>>(new Map());
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);
  const [logTaskId, setLogTaskId] = useState<string | null>(null);

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

  // Straight off the already-polled list — core's activityTrackingStore
  // maintains awaiting_human on every permission_request/question append and
  // clears it on the matching resolution, so this needs no per-task fetch.
  const needsYouIds = useMemo(
    () => new Set(tasks.filter((t) => t.awaitingHuman).map((t) => t.id)),
    [tasks],
  );

  // One transcript fetch per active session, feeding everything both list views
  // show beyond the Task row itself: the todo bar, the in-flight tool line, and
  // — the point of the rewrite — the actual pending decision, rendered inline so
  // a blocked session can be answered without opening it. Scoped to
  // ACTIVE_STATUSES, so it's bounded by the fleet's concurrency cap of 5, on the
  // same 5s cadence as loadTasks. No new RPC: this fetch already existed for the
  // todo bars alone.
  useEffect(() => {
    const active = tasks.filter((t) => ACTIVE_STATUSES.has(t.status));
    if (active.length === 0) {
      setSummaries(new Map());
      return;
    }
    let cancelled = false;
    Promise.all(
      active.map((t) =>
        client
          .getTranscript({ taskId: t.id, sinceSeq: 0n })
          .then((res) => [t.id, listSummary(res.entries)] as const)
          .catch(() => null),
      ),
    ).then((results) => {
      if (cancelled) return;
      setSummaries(new Map(results.filter((r): r is NonNullable<typeof r> => r !== null)));
    });
    return () => {
      cancelled = true;
    };
  }, [tasks]);

  function pushUrl(next: View, taskId: string | null) {
    const url = new URL(window.location.href);
    if (next !== "tasks") url.searchParams.set("view", next);
    else url.searchParams.delete("view");
    if (taskId) url.searchParams.set("task", taskId);
    else url.searchParams.delete("task");
    window.history.pushState({}, "", url);
  }

  function selectTask(id: string) {
    setSelectedId(id);
    setView("tasks");
    pushUrl("tasks", id);
  }

  // The console's detail view is full-width, so unlike the old split-pane
  // layout every form factor now has a real "back to the list".
  function clearSelection() {
    setSelectedId(null);
    pushUrl(view === "tasks" ? "tasks" : view, null);
  }

  function selectView(next: View) {
    setView(next);
    // Leaving the tasks view drops the open session: the nav is between
    // top-level places, and coming back to a stale ?task= would be surprising.
    const keepTask = next === "tasks" ? selectedId : null;
    if (next !== "tasks") setSelectedId(null);
    pushUrl(next, keepTask);
  }

  // Force-tears-down any live session then soft-deletes the task. Confirmation
  // is a ConfirmModal, not window.confirm (native dialogs can't be themed and
  // block the render thread).
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

  const retryTask = useCallback(
    (id: string) => {
      client
        .retryTask({ taskId: id })
        .then(() => loadTasks())
        .catch((err: Error) => setTasksError(err.message));
    },
    [loadTasks],
  );

  // The header's live census. `liveState` is server-derived (docs/adr/0040), so
  // every client agrees on what "working" means.
  const counts = useMemo(() => {
    const waiting = tasks.filter((t) => t.awaitingHuman || t.status === "proposed").length;
    const working = tasks.filter((t) => ACTIVE_STATUSES.has(t.status) && !t.awaitingHuman).length;
    const done = tasks.filter((t) => t.liveState === "done").length;
    return { waiting, working, done, idle: Math.max(0, tasks.length - waiting - working - done) };
  }, [tasks]);

  const repoCount = useMemo(
    () => new Set(tasks.filter((t) => t.kind !== "thot").map((t) => t.repo)).size,
    [tasks],
  );

  const filteredTasks = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return tasks;
    return tasks.filter(
      (t) => t.repo.toLowerCase().includes(q) || t.description.toLowerCase().includes(q),
    );
  }, [tasks, filter]);

  const proposed = useMemo(() => tasks.filter((t) => t.status === "proposed"), [tasks]);

  const shared = {
    tasks: filteredTasks,
    summaries,
    needsYouIds,
    onSelect: selectTask,
    onDelete: deleteTask,
    onRetry: retryTask,
    onOpenLogs: setLogTaskId,
    reload: loadTasks,
  };

  return (
    // h-dvh, not h-screen: 100vh is pinned to the largest possible viewport on
    // mobile browsers, so it doesn't shrink when the keyboard opens and the
    // composer ends up underneath it. h-dvh tracks the visible viewport.
    <div className="h-dvh overflow-hidden bg-base-100 text-base-content flex flex-col">
      {isDesktop ? (
        <div className="flex-none flex items-center gap-4 px-4.5 py-3 border-b border-line bg-base-200">
          <span className="text-[15px] font-semibold tracking-[0.02em] text-primary">herd</span>
          <span className="text-[11.5px] text-dim2 whitespace-nowrap">
            ukubi-cluster · {repoCount} repos
          </span>
          <Segmented value={view} options={NAV} onChange={selectView} className="ml-1" />

          {counts.waiting > 0 && (
            <button
              type="button"
              onClick={() => selectView("tasks")}
              className="flex items-center gap-2 cursor-pointer ml-auto"
            >
              <span className="w-[7px] h-[7px] rounded-full bg-error ring-glow animate-fpulse" />
              <span className="text-[12.5px] font-medium text-error whitespace-nowrap">
                {counts.waiting} waiting on you
              </span>
            </button>
          )}
          <span className={`text-[11.5px] text-dim2 whitespace-nowrap ${counts.waiting > 0 ? "" : "ml-auto"}`}>
            {counts.working} working · {counts.done} done · {counts.idle} idle
          </span>

          {view === "tasks" && (
            <label className="flex items-center gap-2 border border-line px-2.5 py-1 w-[150px] text-[11.5px] text-dim2 focus-within:border-primary/60">
              <span aria-hidden>⌕</span>
              <input
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="filter"
                aria-label="filter sessions"
                className="bg-transparent outline-none min-w-0 flex-1 text-base-content placeholder:text-dim2"
              />
            </label>
          )}
          <NewTaskDialog
            onCreated={(id) => {
              loadTasks();
              selectTask(id);
            }}
          />
          <SettingsMenu theme={theme} onThemeChange={setTheme} />
        </div>
      ) : (
        <div className="flex-none border-b border-line bg-base-200">
          <div className="flex items-center gap-2.5 px-3.5 py-2.5">
            <span className="text-[15px] font-semibold text-primary">herd</span>
            <span className="text-[10.5px] text-dim2">ukubi</span>
            {counts.waiting > 0 && (
              <button
                type="button"
                onClick={() => selectView("tasks")}
                className="ml-auto flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-[3px] cursor-pointer"
              >
                <span className="w-1.5 h-1.5 rounded-full bg-error ring-glow animate-fpulse" />
                <span className="text-[12px] font-medium text-error whitespace-nowrap">
                  {counts.waiting} waiting
                </span>
              </button>
            )}
            <button
              type="button"
              onClick={() => setSearchOpen((v) => !v)}
              aria-label="Filter sessions"
              aria-expanded={searchOpen}
              className={`text-[14px] px-1 ${counts.waiting > 0 ? "" : "ml-auto"} ${searchOpen ? "text-primary" : "text-dim"}`}
            >
              ⌕
            </button>
            <NewTaskDialog
              compact
              onCreated={(id) => {
                loadTasks();
                selectTask(id);
              }}
            />
            <SettingsMenu theme={theme} onThemeChange={setTheme} />
          </div>
          {searchOpen && (
            <div className="px-3.5 pb-2.5">
              <input
                autoFocus
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="filter sessions"
                aria-label="filter sessions"
                className="w-full border border-line bg-transparent px-2.5 py-2 text-[12px] outline-none focus:border-primary/60 placeholder:text-dim2"
              />
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
      <LogDrawer taskId={logTaskId} onClose={() => setLogTaskId(null)} />

      {/* min-w-0 alongside min-h-0: a flex item's min-width defaults to auto, so
          any descendant with a large min-content width (a long URL, a nowrap
          string) silently widens this past the viewport instead of clipping. */}
      <div className="flex-1 min-h-0 min-w-0 flex flex-col">
        {view === "worktrees" ? (
          <Worktrees onSelectTask={selectTask} />
        ) : view === "files" ? (
          <Files />
        ) : view === "audits" ? (
          <Audits proposed={proposed} onSelectTask={selectTask} reloadTasks={loadTasks} />
        ) : selectedId ? (
          isDesktop ? (
            <TaskDetail taskId={selectedId} tasks={tasks} onBack={clearSelection} onClosed={clearSelection} />
          ) : (
            <MobileTaskDetail taskId={selectedId} onBack={clearSelection} onDelete={() => deleteTask(selectedId)} />
          )
        ) : isDesktop ? (
          <TaskList {...shared} />
        ) : (
          <MobileTaskList {...shared} />
        )}
      </div>

      {!isDesktop && (
        <Segmented
          value={view}
          options={MOBILE_NAV}
          onChange={selectView}
          grow
          size="lg"
          className="flex-none border-x-0 border-b-0 border-t"
        />
      )}
    </div>
  );
}
