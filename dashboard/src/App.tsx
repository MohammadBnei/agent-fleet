import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { SessionList, ACTIVE_STATES } from "./pages/SessionList";
import { SessionDetail } from "./pages/SessionDetail";
import { Files } from "./pages/Files";
import { Schedules } from "./pages/Schedules";
import { Observability } from "./pages/Observability";
import { NewSessionDialog } from "./components/NewSessionDialog";
import { SettingsMenu } from "./components/SettingsMenu";
import { NeedsYouModal } from "./components/NeedsYouModal";
import { Segmented } from "./components/Segmented";
import { MobileSessionList } from "./mobile/MobileSessionList";
import { MobileSessionDetail } from "./mobile/MobileSessionDetail";
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
import { useFeedWidth } from "./useFeedWidth";

// No router library (see docs/adr/0013's plan) — state mirrored to
// ?view=/?session= so a session is still bookmarkable/shareable without pulling
// in react-router for this surface.
//
// ?task= is still read, and ?view=tasks still resolves: every link shared
// before docs/adr/0048's rename carries them, and a bookmark that silently
// opens the wrong thing is worse than one that 404s. Only ?session= is
// written, so a legacy param disappears the first time the URL is pushed.
function readSessionIdFromUrl(): string | null {
  const p = new URLSearchParams(window.location.search);
  return p.get("session") ?? p.get("task");
}

export type View = "sessions" | "schedules" | "files" | "observability";

// The manifest's app shortcuts (icons/site.webmanifest) land here. Read once on
// mount: they're an entry point, not persistent state, and the params are
// dropped from the URL as soon as they've been honoured so a refresh doesn't
// re-trigger them.
function readShortcut(): { needsYouOnly: boolean; newSession: boolean } {
  const p = new URLSearchParams(window.location.search);
  return { needsYouOnly: p.get("filter") === "blocked", newSession: p.get("new") === "1" };
}
function readViewFromUrl(): View {
  const v = new URLSearchParams(window.location.search).get("view");
  // "audits" is the pre-rename name of this view: a schedule was an audit
  // until its repo stopped being a constant. "tasks" is the pre-rename name of
  // the default one and falls through to it — listed so it reads as deliberate.
  if (v === "audits") return "schedules";
  return v === "files" || v === "schedules" || v === "observability" ? v : "sessions";
}

const NAV: readonly { value: View; label: string }[] = [
  { value: "sessions", label: "sessions" },
  { value: "schedules", label: "schedules" },
  { value: "files", label: "files" },
  { value: "observability", label: "observability" },
];

// Mobile's bottom bar has less room; "trees" is the console mockup's own label.
const MOBILE_NAV: readonly { value: View; label: string }[] = [
  { value: "sessions", label: "sessions" },
  { value: "schedules", label: "sched" },
  { value: "files", label: "files" },
  // Same shortening rule the "trees" label above follows — the bottom bar
  // now carries five cells and has no room for the full word.
  { value: "observability", label: "obs" },
];

// Plain polling, not a stream — a second live feed just for the list is
// unjustified scope (docs/adr/0014); the detail view's StreamTranscript RPC is
// where "live" actually matters. Lives here rather than inside SessionList so the
// detail view can share the same fetched list instead of polling again.
const POLL_INTERVAL_MS = 5000;

// Transcript entries fetched per session for the list summary — the newest
// ones. Matches useSessionDetail's PAGE.
const SUMMARY_PAGE = 200;

export default function App() {
  // Matches Tailwind's `sm:`. Desktop and mobile detail views used to both
  // mount, CSS-hidden — each ran its own useSessionDetail, so two independent
  // StreamTranscript subscriptions polled the same session concurrently whenever
  // one was open. Gating the mount on the real viewport is what stops that.
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [theme, setTheme] = useTheme();
  const [feedWidth, setFeedWidth] = useFeedWidth();
  // The header badge opens this instead of navigating. See NeedsYouModal.
  const [decisionsOpen, setDecisionsOpen] = useState(false);
  const [view, setView] = useState<View>(readViewFromUrl);
  const [selectedId, setSelectedId] = useState<string | null>(readSessionIdFromUrl);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [proposals, setProposals] = useState<Proposal[]>([]);
  const [sessionsError, setSessionsError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [summaries, setSummaries] = useState<Map<string, ListSummary>>(new Map());
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);
  const [logSessionId, setLogSessionId] = useState<string | null>(null);
  const [shortcut] = useState(readShortcut);
  const [needsYouOnly, setNeedsYouOnly] = useState(shortcut.needsYouOnly);

  // Honour the shortcut, then scrub its params so a reload lands on the plain
  // console rather than silently re-filtering or reopening the form.
  useEffect(() => {
    if (!shortcut.needsYouOnly && !shortcut.newSession) return;
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
  // Search is server-side now (Postgres FTS over session labels + transcript
  // text), so the query rides the poll. Debounced ~250ms so each keystroke
  // isn't its own round trip; an empty query is the unfiltered listing.
  const [debouncedFilter, setDebouncedFilter] = useState("");
  useEffect(() => {
    const id = setTimeout(() => setDebouncedFilter(filter.trim()), 250);
    return () => clearTimeout(id);
  }, [filter]);

  const pollFailures = useRef(0);
  const loadSessions = useCallback(() => {
    return client
      .listSessions({ query: debouncedFilter })
      .then((res) => {
        pollFailures.current = 0;
        setSessions(res.sessions);
      })
      .catch((err: Error) => {
        if (++pollFailures.current >= 2) setSessionsError(err.message);
      });
  }, [debouncedFilter]);

  useEffect(() => pollVisible(loadSessions, POLL_INTERVAL_MS), [loadSessions]);

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
  // clears it on the matching resolution, so this needs no per-session fetch.
  // Sessions holding an unanswered QUESTION, from the summaries above. A
  // question survives its pod (docs/adr/0050) and answering one warms a fresh
  // pod to receive the answer, so these stay answerable in the list whatever
  // pod_phase says. A stranded PERMISSION does not — see bucketSessions.
  const answerableIds = useMemo(
    () =>
      new Set(
        [...summaries.entries()].filter(([, sum]) => sum.pendingQuestion !== null).map(([id]) => id),
      ),
    [summaries],
  );

  const needsYouIds = useMemo(
    () => new Set(sessions.filter((t) => t.pendingDecisions > 0).map((t) => t.id)),
    [sessions],
  );

  // One transcript fetch per active session, feeding everything both list views
  // show beyond the Session row itself: the todo bar, the in-flight tool line, and
  // — the point of the rewrite — the actual pending decision, rendered inline so
  // a blocked session can be answered without opening it. Scoped to
  // ACTIVE_STATES, so it's bounded by the fleet's concurrency cap of 5, on the
  // same 5s cadence as loadSessions. No new RPC: this fetch already existed for the
  // todo bars alone.
  useEffect(() => {
    // Active sessions, PLUS any session still holding an unanswered decision
    // whatever its pod is doing. A session whose pod died mid-question has no
    // live state at all, so it fell out of this fetch — and with no summary the
    // STUCK row had nothing to show, which is how a question could exist on the
    // wire and be invisible in the console.
    //
    // Still bounded: active sessions are capped by the fleet's concurrency
    // limit, and the extra ones are sliced, so a long tail of abandoned
    // decisions cannot turn one poll into a hundred transcript fetches.
    const active = sessions.filter((t) => ACTIVE_STATES.has(t.liveState));
    const strandedCap = 10;
    const stranded = sessions
      .filter((t) => !ACTIVE_STATES.has(t.liveState) && t.pendingDecisions > 0 && t.archivedAt === undefined)
      .slice(0, strandedCap);
    const wanted = [...active, ...stranded];
    if (wanted.length === 0) {
      setSummaries(new Map());
      return;
    }
    let cancelled = false;
    Promise.all(
      wanted.map((t) =>
        client
          // limit, so the window is the NEWEST entries. Without it,
          // transcriptWindow reads FORWARD from sinceSeq 0 — the OLDEST 1000
          // (core/internal/dashboard/server.go, and its own test pins that
          // shape). Past a thousand entries this fetch could no longer see a
          // pending decision at all, while the detail view still could,
          // because that one has always passed a limit. So a long-running
          // session's question was on the wire, counted by core, rendering a
          // blocked card — with nothing inside it. Reported live 2026-08-19 as
          // "I can't see questions now", and "new questions don't show either"
          // is the same fact: a new question lands at the HIGH end, which is
          // exactly the part this was never reading.
          //
          // 200 matches the detail view. A summary only needs the tail: the
          // pending decision, the latest todos, the in-flight tool.
          .getTranscript({ sessionId: t.id, limit: SUMMARY_PAGE })
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
  }, [sessions]);

  function pushUrl(next: View, sessionId: string | null) {
    const url = new URL(window.location.href);
    if (next !== "sessions") url.searchParams.set("view", next);
    else url.searchParams.delete("view");
    if (sessionId) url.searchParams.set("session", sessionId);
    else url.searchParams.delete("session");
    // Unconditionally, so a legacy ?task= that was just resolved into
    // selectedId does not linger alongside the ?session= that replaced it —
    // two ids in one URL is the kind of thing that resolves differently in
    // the next reader.
    url.searchParams.delete("task");
    window.history.pushState({}, "", url);
  }

  function selectSession(id: string) {
    setSelectedId(id);
    setView("sessions");
    pushUrl("sessions", id);
  }

  // The console's detail view is full-width, so unlike the old split-pane
  // layout every form factor now has a real "back to the list".
  function clearSelection() {
    setSelectedId(null);
    pushUrl(view === "sessions" ? "sessions" : view, null);
  }

  function selectView(next: View) {
    setView(next);
    // Leaving the sessions view drops the open session: the nav is between
    // top-level places, and coming back to a stale ?session= would be surprising.
    const keepSession = next === "sessions" ? selectedId : null;
    if (next !== "sessions") setSelectedId(null);
    pushUrl(next, keepSession);
  }

  // Force-tears-down any live session then soft-deletes the session. Confirmation
  // is a ConfirmModal, not window.confirm (native dialogs can't be themed and
  // block the render thread).
  function deleteSession(id: string) {
    setPendingDeleteId(id);
  }

  function confirmDeleteSession() {
    const id = pendingDeleteId;
    setPendingDeleteId(null);
    if (!id) return;
    client
      .deleteSession({ sessionId: id })
      .then(() => {
        loadSessions();
        if (id === selectedId) clearSelection();
      })
      .catch((err: Error) => setSessionsError(err.message));
  }

  // retryTask used to live here, wired to a "retry" button on every crashed
  // row. It was `void id;` — retry died with failed_permanently
  // (docs/adr/0048), since nothing reclaims a session into a dead state any
  // more and retrying is just sending it another message. Both buttons are
  // gone with it; a control that silently does nothing is worse than no
  // control.

  // The header's live census. `liveState` is server-derived (docs/adr/0040), so
  // every client agrees on what "working" means.
  // The badge's own predicate, once. The modal renders exactly what the badge
  // counted — deriving it twice is how a "3 waiting" button opens an empty box.
  //
  // Sorted longest-waiting first, matching bucketSessions' needsYou order. The
  // modal does not go through bucketSessions, so it does NOT inherit that sort
  // for free — it listed a 8m-old decision above a 53m-old one while the list
  // two inches behind it showed the opposite, which is the kind of disagreement
  // that makes a human stop trusting the ordering entirely.
  const blockedSessions = useMemo(
    () =>
      sessions
        .filter((t) => t.archivedAt === undefined && t.liveState === "blocked")
        .sort(
          (a, b) =>
            (a.lastActiveAt ? new Date(a.lastActiveAt).getTime() : 0) -
            (b.lastActiveAt ? new Date(b.lastActiveAt).getTime() : 0),
        ),
    [sessions],
  );

  const counts = useMemo(() => {
    // Same rule the needsYou bucket uses, and for the same reason: the raw
    // pendingDecisions count counts sessions whose pod is gone (and archived
    // ones), so the badge would show work waiting on a human that no human
    // can act on. "blocked" is live-and-pending, already derived server-side.
    const waiting = blockedSessions.length;
    // `working` means the badge says WORKING, not "has a live pod".
    // ACTIVE_STATES is the pod-liveness set — it includes idle, unknown and
    // stalled — so counting it here reported "1 working · 0 idle" for a fleet
    // whose only live session was rendering an IDLE badge two lines below.
    // The header and the row have to agree; they are read together.
    const working = sessions.filter((t) => t.liveState === "working").length;
    const done = sessions.filter((t) => t.liveState === "done").length;
    return { waiting, working, done, idle: Math.max(0, sessions.length - waiting - working - done) };
  }, [sessions, blockedSessions]);

  const repoCount = useMemo(
    () => new Set(sessions.filter(() => true).map((t) => t.repo)).size,
    [sessions],
  );

  const filteredSessions = useMemo(() => {
    // needsYouOnly is the manifest's "Waiting on you" shortcut. Mobile already
    // opens on its needs-you bucket, so this only changes the desktop list —
    // but it narrows the shared array either way, which keeps the two honest.
    //
    // Same predicate as the needsYou bucket and the header count, and for the
    // same reason: pendingDecisions is a raw count that stays above zero
    // forever when a pod dies mid-decision, so filtering on it showed sessions
    // nobody can act on — and hid nothing, since the shortcut's whole promise
    // is "these are waiting on you".
    // Text search is server-side now (the poll passes `query`), so this only
    // applies the needsYouOnly live-state shortcut — a predicate the server
    // doesn't know about.
    return needsYouOnly
      ? sessions.filter((t) => t.archivedAt === undefined && t.liveState === "blocked")
      : sessions;
  }, [sessions, needsYouOnly]);


  const shared = {
    answerableIds,
    sessions: filteredSessions,
    summaries,
    needsYouIds,
    onSelect: selectSession,
    onDelete: deleteSession,
    onOpenLogs: setLogSessionId,
    reload: loadSessions,
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
                // Opens the decisions over whatever is on screen rather than
                // navigating to the list. Answering one is a 5-second
                // interruption; it should not cost the reader their place —
                // and from inside a session detail the old handler was a no-op
                // anyway, since selectView("sessions") keeps the open session.
                onClick={() => setDecisionsOpen(true)}
                title="Answer every pending decision without leaving this page"
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

          {view === "sessions" && (
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
          <NewSessionDialog
            autoOpen={shortcut.newSession}
            onCreated={(id) => {
              loadSessions();
              selectSession(id);
            }}
          />
          <SettingsMenu theme={theme} onThemeChange={setTheme} feedWidth={feedWidth} onFeedWidthChange={setFeedWidth} />
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
                onClick={() => setDecisionsOpen(true)}
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
            <NewSessionDialog
              compact
              autoOpen={shortcut.newSession}
              onCreated={(id) => {
                loadSessions();
                selectSession(id);
              }}
            />
            <SettingsMenu theme={theme} onThemeChange={setTheme} feedWidth={feedWidth} onFeedWidthChange={setFeedWidth} />
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

      <ErrorModal message={sessionsError} onClose={() => setSessionsError(null)} />
      <ConfirmModal
        open={pendingDeleteId !== null}
        message="Delete this session? This tears down any live pod and removes it from the list."
        onConfirm={confirmDeleteSession}
        onCancel={() => setPendingDeleteId(null)}
      />
      <LogDrawer sessionId={logSessionId} onClose={() => setLogSessionId(null)} />
      <NeedsYouModal
        open={decisionsOpen}
        sessions={blockedSessions}
        summaries={summaries}
        onClose={() => setDecisionsOpen(false)}
        onOpenSession={(id) => {
          // Close first: a plan sends you into the session, and leaving this
          // open behind the detail view means answering there and then finding
          // a stale copy of the same decision still sitting on top.
          setDecisionsOpen(false);
          selectSession(id);
        }}
        reload={loadSessions}
      />

      {/* min-w-0 alongside min-h-0: a flex item's min-width defaults to auto, so
          any descendant with a large min-content width (a long URL, a nowrap
          string) silently widens this past the viewport instead of clipping. */}
      <div className="flex-1 min-h-0 min-w-0 flex flex-col">
        {/*
          The worktrees view is gone (docs/adr/0048 §5). It listed linked git
          worktrees on one shared PVC and offered to delete them by hand; there
          are no worktrees now, a session's tree is its own PVC, and the
          retention GC reclaims it without anyone clicking.
        */}
        {view === "files" ? (
          <Files />
        ) : view === "schedules" ? (
          <Schedules
            proposals={proposals}
            onSelectSession={selectSession}
            reloadSessions={() => {
              void loadSessions();
              void loadProposals();
            }}
          />
        ) : view === "observability" ? (
          <Observability onSelectSession={selectSession} />
        ) : selectedId ? (
          isDesktop ? (
            <SessionDetail sessionId={selectedId} sessions={sessions} onClosed={clearSelection} />
          ) : (
            <MobileSessionDetail sessionId={selectedId} onBack={clearSelection} onDelete={() => deleteSession(selectedId)} />
          )
        ) : isDesktop ? (
          <SessionList {...shared} />
        ) : (
          <MobileSessionList {...shared} />
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
