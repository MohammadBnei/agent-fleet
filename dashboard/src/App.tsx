import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { TaskList, ACTIVE_STATES } from "./pages/TaskList";
import { TaskDetail } from "./pages/TaskDetail";
import { Worktrees } from "./pages/Worktrees";
import { Files } from "./pages/Files";
import { Audits } from "./pages/Audits";
import { Observability } from "./pages/Observability";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { SettingsMenu } from "./components/SettingsMenu";
import { Segmented } from "./components/Segmented";
import { MobileTaskList } from "./mobile/MobileTaskList";
import { MobileTaskDetail } from "./mobile/MobileTaskDetail";
import { client } from "./connectClient";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import type { Proposal } from "./gen/agentfleet/v1/dashboard_pb";
import { listSummary, type ListSummary } from "./transcript";
import { ErrorModal } from "./components/ErrorModal";
import { ConfirmModal } from "./components/ConfirmModal";
import { LogDrawer } from "./components/LogDrawer";
import { useMediaQuery } from "./useMediaQuery";
import { pollVisible } from "./pollVisible";
import { useTheme } from "./useTheme";

// No router library (see docs/adr/0013's plan) — state mirrored to
// ?view=/?task= so a session is still bookmarkable/shareable without pulling
// in react-router for this surface.
function readTaskIdFromUrl(): string | null {
  return new URLSearchParams(window.location.search).get("task");
}

export type View = "tasks" | "audits" | "worktrees" | "files" | "observability";

// The manifest's app shortcuts (icons/site.webmanifest) land here. Read once on
// mount: they're an entry point, not persistent state, and the params are
// dropped from the URL as soon as they've been honoured so a refresh doesn't
// re-trigger them.
function readShortcut(): { needsYouOnly: boolean; newTask: boolean } {
  const p = new URLSearchParams(window.location.search);
  return { needsYouOnly: p.get("filter") === "blocked", newTask: p.get("new") === "1" };
}

function readViewFromUrl(): View {
  const v = new URLSearchParams(window.location.search).get("view");
  return v === "worktrees" || v === "files" || v === "audits" || v === "observability" ? v : "tasks";
}

const NAV: readonly { value: View; label: string }[] = [
  { value: "tasks", label: "tasks" },
  { value: "audits", label: "audits" },
  { value: "worktrees", label: "worktrees" },
  { value: "files", label: "files" },
  { value: "observability", label: "observability" },
];

// Mobile's bottom bar has less room; "trees" is the console mockup's own label.
const MOBILE_NAV: readonly { value: View; label: string }[] = [
  { value: "tasks", label: "tasks" },
  { value: "audits", label: "audits" },
  { value: "worktrees", label: "trees" },
  { value: "files", label: "files" },
  // Same shortening rule the "trees" label above follows — the bottom bar
  // now carries five cells and has no room for the full word.
  { value: "observability", label: "obs" },
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
  const [tasks, setTasks] = useState<Session[]>([]);
  const [proposals, setProposals] = useState<Proposal[]>([]);
  const [tasksError, setTasksError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [summaries, setSummaries] = useState<Map<string, ListSummary>>(new Map());
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);
  const [logTaskId, setLogTaskId] = useState<string | null>(null);
  const [shortcut] = useState(readShortcut);
  const [needsYouOnly, setNeedsYouOnly] = useState(shortcut.needsYouOnly);

  // Honour the shortcut, then scrub its params so a reload lands on the plain
  // console rather than silently re-filtering or reopening the form.
  useEffect(() => {
    if (!shortcut.needsYouOnly && !shortcut.newTask) return;
    const url = new URL(window.location.href);
    url.searchParams.delete("filter");
    url.searchParams.delete("new");
    window.history.replaceState({}, "", url);
  }, [shortcut]);

  // A single failed poll is not news: this runs every 5s, the next one
  // almost always succeeds, and a modal for each one made the console
  // unusable on a phone with patchy signal (or one that had just been
  // unlocked — see pollVisible). Two consecutive failures is 10s of real
  // outage, which is worth interrupting for. User-initiated actions below
  // (delete, retry) still surface on the very first failure — someone is
  // watching for that result. Deliberately no auto-clear on success: the
  // same modal carries those action errors, and a poll succeeding 5s later
  // would yank a delete failure off the screen before it was read.
  const pollFailures = useRef(0);
  const loadTasks = useCallback(() => {
    return client
      .listSessions({})
      .then((res) => {
        pollFailures.current = 0;
        setTasks(res.sessions);
      })
      .catch((err: Error) => {
        if (++pollFailures.current >= 2) setTasksError(err.message);
      });
  }, []);

  useEffect(() => pollVisible(loadTasks, POLL_INTERVAL_MS), [loadTasks]);

  // Proposals are a separate table with no pod path of their own
  // (docs/adr/0048), so they need their own fetch rather than being filtered
  // out of the session list the way `status = 'proposed'` used to be.
  //
  // A failure here is deliberately quiet: an audit suggestion not appearing is
  // not worth the modal that a failing session poll gets, and the next tick
  // retries.
  const loadProposals = useCallback(() => {
    return client
      .listProposals({})
      .then((res) => setProposals(res.proposals))
      .catch(() => {});
  }, []);

  useEffect(() => pollVisible(loadProposals, POLL_INTERVAL_MS), [loadProposals]);

  // Straight off the already-polled list — core's activityTrackingStore
  // maintains awaiting_human on every permission_request/question append and
  // clears it on the matching resolution, so this needs no per-task fetch.
  const needsYouIds = useMemo(
    () => new Set(tasks.filter((t) => t.pendingDecisions > 0).map((t) => t.id)),
    [tasks],
  );

  // One transcript fetch per active session, feeding everything both list views
  // show beyond the Task row itself: the todo bar, the in-flight tool line, and
  // — the point of the rewrite — the actual pending decision, rendered inline so
  // a blocked session can be answered without opening it. Scoped to
  // ACTIVE_STATES, so it's bounded by the fleet's concurrency cap of 5, on the
  // same 5s cadence as loadTasks. No new RPC: this fetch already existed for the
  // todo bars alone.
  useEffect(() => {
    const active = tasks.filter((t) => ACTIVE_STATES.has(t.liveState));
    if (active.length === 0) {
      setSummaries(new Map());
      return;
    }
    let cancelled = false;
    Promise.all(
      active.map((t) =>
        client
          .getTranscript({ sessionId: t.id, sinceSeq: 0n })
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

  function pushUrl(next: View, sessionId: string | null) {
    const url = new URL(window.location.href);
    if (next !== "tasks") url.searchParams.set("view", next);
    else url.searchParams.delete("view");
    if (sessionId) url.searchParams.set("task", sessionId);
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
      .deleteSession({ sessionId: id })
      .then(() => {
        loadTasks();
        if (id === selectedId) clearSelection();
      })
      .catch((err: Error) => setTasksError(err.message));
  }

  const retryTask = useCallback(
    (id: string) => {
      // Retry is deleted with failed_permanently — there is no dead state to
      // resurrect a session from now. Sending it another message is the retry.
      void id;
    },
    [loadTasks],
  );

  // The header's live census. `liveState` is server-derived (docs/adr/0040), so
  // every client agrees on what "working" means.
  const counts = useMemo(() => {
    const waiting = tasks.filter((t) => t.pendingDecisions > 0).length;
    const working = tasks.filter((t) => ACTIVE_STATES.has(t.liveState) && t.pendingDecisions === 0).length;
    const done = tasks.filter((t) => t.liveState === "done").length;
    return { waiting, working, done, idle: Math.max(0, tasks.length - waiting - working - done) };
  }, [tasks]);

  const repoCount = useMemo(
    () => new Set(tasks.filter(() => true).map((t) => t.repo)).size,
    [tasks],
  );

  const filteredTasks = useMemo(() => {
    // needsYouOnly is the manifest's "Waiting on you" shortcut. Mobile already
    // opens on its needs-you bucket, so this only changes the desktop list —
    // but it narrows the shared array either way, which keeps the two honest.
    const base = needsYouOnly
      ? tasks.filter((t) => t.pendingDecisions > 0)
      : tasks;
    const q = filter.trim().toLowerCase();
    if (!q) return base;
    return base.filter(
      (t) => t.repo.toLowerCase().includes(q) || t.description.toLowerCase().includes(q),
    );
  }, [tasks, filter, needsYouOnly]);


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
          <a href="/" className="flex items-center gap-2 text-lg font-semibold tracking-[0.02em] text-primary hover:opacity-80">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="18" height="18" className="flex-none">
              <line x1="32" y1="32" x2="49.85" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
              <line x1="32" y1="32" x2="32" y2="52.61" stroke="#f2749b" strokeOpacity="0.7" strokeWidth="2.65"></line>
              <line x1="32" y1="32" x2="14.15" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
              <line x1="32" y1="32" x2="14.15" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
              <line x1="32" y1="32" x2="32" y2="11.39" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
              <line x1="32" y1="32" x2="49.85" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
              <polygon points="49.85,51.14 42.2,46.72 42.2,37.89 49.85,33.47 57.5,37.89 57.5,46.72" fill="#7d6fae"></polygon>
              <polygon points="32,61.44 24.35,57.02 24.35,48.19 32,43.78 39.65,48.19 39.65,57.02" fill="#f2749b"></polygon>
              <polygon points="14.15,51.14 6.5,46.72 6.5,37.89 14.15,33.47 21.8,37.89 21.8,46.72" fill="#7d6fae"></polygon>
              <polygon points="14.15,30.53 6.5,26.11 6.5,17.28 14.15,12.86 21.8,17.28 21.8,26.11" fill="#7d6fae"></polygon>
              <polygon points="32,20.22 24.35,15.81 24.35,6.98 32,2.56 39.65,6.98 39.65,15.81" fill="#7d6fae"></polygon>
              <polygon points="49.85,30.53 42.2,26.11 42.2,17.28 49.85,12.86 57.5,17.28 57.5,26.11" fill="#7d6fae"></polygon>
              <polygon points="32,40.83 24.35,36.42 24.35,27.58 32,23.17 39.65,27.58 39.65,36.42" fill="currentColor"></polygon>
            </svg>
            herd
          </a>
          <span className="text-xs text-dim2 whitespace-nowrap">
            ukubi-cluster · {repoCount} repos
          </span>
          <Segmented value={view} options={NAV} onChange={selectView} className="ml-1" />

          {(counts.waiting > 0 || needsYouOnly) && (
            <div className="flex items-center gap-2 ml-auto">
              <button
                type="button"
                onClick={() => {
                  selectView("tasks");
                  setNeedsYouOnly(true);
                }}
                className="flex items-center gap-2 cursor-pointer"
              >
                <span className="w-[7px] h-[7px] rounded-full bg-error ring-glow animate-fpulse" />
                <span className="text-sm font-medium text-error whitespace-nowrap">
                  {counts.waiting} waiting on you
                </span>
              </button>
              {/* A filter with no visible way out is a trap — especially when an
                  app shortcut, not a click, is what turned it on. */}
              {needsYouOnly && (
                <button
                  type="button"
                  onClick={() => setNeedsYouOnly(false)}
                  title="Show every session"
                  aria-label="Clear the waiting-on-you filter"
                  className="text-xs text-dim2 hover:text-base-content border border-line px-1.5 py-0.5"
                >
                  only ✕
                </button>
              )}
            </div>
          )}
          <span className={`text-xs text-dim2 whitespace-nowrap ${counts.waiting > 0 ? "" : "ml-auto"}`}>
            {counts.working} working · {counts.done} done · {counts.idle} idle
          </span>

          {view === "tasks" && (
            <label className="flex items-center gap-2 border border-line px-2.5 py-1 w-[150px] text-xs text-dim2 focus-within:border-primary/60">
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
            autoOpen={shortcut.newTask}
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
            <a href="/" className="flex items-center gap-1.5 text-lg font-semibold text-primary">
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="16" height="16" className="flex-none">
                <line x1="32" y1="32" x2="49.85" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
                <line x1="32" y1="32" x2="32" y2="52.61" stroke="#f2749b" strokeOpacity="0.7" strokeWidth="2.65"></line>
                <line x1="32" y1="32" x2="14.15" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
                <line x1="32" y1="32" x2="14.15" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
                <line x1="32" y1="32" x2="32" y2="11.39" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
                <line x1="32" y1="32" x2="49.85" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
                <polygon points="49.85,51.14 42.2,46.72 42.2,37.89 49.85,33.47 57.5,37.89 57.5,46.72" fill="#7d6fae"></polygon>
                <polygon points="32,61.44 24.35,57.02 24.35,48.19 32,43.78 39.65,48.19 39.65,57.02" fill="#f2749b"></polygon>
                <polygon points="14.15,51.14 6.5,46.72 6.5,37.89 14.15,33.47 21.8,37.89 21.8,46.72" fill="#7d6fae"></polygon>
                <polygon points="14.15,30.53 6.5,26.11 6.5,17.28 14.15,12.86 21.8,17.28 21.8,26.11" fill="#7d6fae"></polygon>
                <polygon points="32,20.22 24.35,15.81 24.35,6.98 32,2.56 39.65,6.98 39.65,15.81" fill="#7d6fae"></polygon>
                <polygon points="49.85,30.53 42.2,26.11 42.2,17.28 49.85,12.86 57.5,17.28 57.5,26.11" fill="#7d6fae"></polygon>
                <polygon points="32,40.83 24.35,36.42 24.35,27.58 32,23.17 39.65,27.58 39.65,36.42" fill="currentColor"></polygon>
              </svg>
              herd
            </a>
            <span className="text-2xs text-dim2">ukubi</span>
            {counts.waiting > 0 && (
              <button
                type="button"
                onClick={() => selectView("tasks")}
                className="ml-auto flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-[3px] cursor-pointer"
              >
                <span className="w-1.5 h-1.5 rounded-full bg-error ring-glow animate-fpulse" />
                <span className="text-sm font-medium text-error whitespace-nowrap">
                  {counts.waiting} waiting
                </span>
              </button>
            )}
            <button
              type="button"
              onClick={() => setSearchOpen((v) => !v)}
              aria-label="Filter sessions"
              aria-expanded={searchOpen}
              className={`text-base px-1 ${counts.waiting > 0 ? "" : "ml-auto"} ${searchOpen ? "text-primary" : "text-dim"}`}
            >
              ⌕
            </button>
            <NewTaskDialog
              compact
              autoOpen={shortcut.newTask}
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
                className="w-full border border-line bg-transparent px-2.5 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2"
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
      <LogDrawer sessionId={logTaskId} onClose={() => setLogTaskId(null)} />

      {/* min-w-0 alongside min-h-0: a flex item's min-width defaults to auto, so
          any descendant with a large min-content width (a long URL, a nowrap
          string) silently widens this past the viewport instead of clipping. */}
      <div className="flex-1 min-h-0 min-w-0 flex flex-col">
        {view === "worktrees" ? (
          <Worktrees onSelectTask={selectTask} />
        ) : view === "files" ? (
          <Files />
        ) : view === "audits" ? (
          <Audits
            proposals={proposals}
            onSelectTask={selectTask}
            reloadTasks={() => {
              void loadTasks();
              void loadProposals();
            }}
          />
        ) : view === "observability" ? (
          <Observability onSelectTask={selectTask} />
        ) : selectedId ? (
          isDesktop ? (
            <TaskDetail sessionId={selectedId} tasks={tasks} onBack={clearSelection} onClosed={clearSelection} />
          ) : (
            <MobileTaskDetail sessionId={selectedId} onBack={clearSelection} onDelete={() => deleteTask(selectedId)} />
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
