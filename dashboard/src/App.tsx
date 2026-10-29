import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ConfirmModal } from "./components/ConfirmModal";
import { ErrorModal } from "./components/ErrorModal";
import { LogDrawer } from "./components/LogDrawer";
import { Logo } from "./components/Logo";
import { NeedsYouModal } from "./components/NeedsYouModal";
import { NewSessionDialog } from "./components/NewSessionDialog";
import { Segmented } from "./components/Segmented";
import { client } from "./connectClient";
import { type DeckItem, deckItems, fetchWithdrawn } from "./decisions";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import type { Proposal } from "./gen/agentfleet/v1/dashboard_pb";
import { MobileSessionDetail } from "./mobile/MobileSessionDetail";
import { MobileSessionList } from "./mobile/MobileSessionList";
import { Files } from "./pages/Files";
import { Observability } from "./pages/Observability";
import { Schedules } from "./pages/Schedules";
import { SessionDetail } from "./pages/SessionDetail";
import { SessionList } from "./pages/SessionList";
import { Settings } from "./pages/Settings";
import { pollVisible } from "./pollVisible";
import { LIVE_STATES } from "./sessionState";
import {
  findPendingPermissions,
  type ListSummary,
  listSummary,
  summaryAnswerable,
} from "./transcript";
import { useDecisionDeck } from "./useDecisionDeck";
import { useFeedWidth } from "./useFeedWidth";
import { useMediaQuery } from "./useMediaQuery";
import { useTheme } from "./useTheme";

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

// The ⚙ in both headers: opens the Settings page.
function SettingsButton({
  active,
  onClick,
}: {
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-label="Settings"
      aria-current={active ? "page" : undefined}
      onClick={onClick}
      className={`flex-none cursor-pointer px-1.5 py-1 text-base hover:text-base-content ${active ? "text-primary" : "text-dim"}`}
    >
      ⚙
    </button>
  );
}

export type View =
  | "sessions"
  | "schedules"
  | "files"
  | "observability"
  | "settings";

// The manifest's app shortcuts (icons/site.webmanifest) land here. Read once on
// mount: they're an entry point, not persistent state, and the params are
// dropped from the URL as soon as they've been honoured so a refresh doesn't
// re-trigger them.
function readShortcut(): { needsYouOnly: boolean; newSession: boolean } {
  const p = new URLSearchParams(window.location.search);
  return {
    needsYouOnly: p.get("filter") === "blocked",
    newSession: p.get("new") === "1",
  };
}
function readViewFromUrl(): View {
  const v = new URLSearchParams(window.location.search).get("view");
  // "audits" is the pre-rename name of this view: a schedule was an audit
  // until its repo stopped being a constant. "tasks" is the pre-rename name of
  // the default one and falls through to it — listed so it reads as deliberate.
  if (v === "audits") return "schedules";
  return v === "files" ||
    v === "schedules" ||
    v === "observability" ||
    v === "settings"
    ? v
    : "sessions";
}

const NAV: readonly { value: View; label: string }[] = [
  { value: "sessions", label: "sessions" },
  { value: "schedules", label: "schedules" },
  { value: "files", label: "files" },
  { value: "observability", label: "observability" },
];

// Mobile's bottom bar has less room for full words.
const MOBILE_NAV: readonly { value: View; label: string }[] = [
  { value: "sessions", label: "sessions" },
  { value: "schedules", label: "sched" },
  { value: "files", label: "files" },
  { value: "observability", label: "obs" },
];

// Proposals are a human gate like any pending decision, but they live on the
// schedules view, and nothing outside it said they existed — 24 alerts sat
// waiting for approval behind a tab whose name promises cron jobs.
function withProposalCount(
  nav: readonly { value: View; label: string }[],
  count: number,
) {
  return count === 0
    ? nav
    : nav.map((o) =>
        o.value === "schedules" ? { ...o, label: `${o.label} · ${count}` } : o,
      );
}

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
  const [selectedId, setSelectedId] = useState<string | null>(
    readSessionIdFromUrl,
  );
  const [sessions, setSessions] = useState<Session[]>([]);
  const [sessionsLoaded, setSessionsLoaded] = useState(false);
  const [proposals, setProposals] = useState<Proposal[]>([]);
  const [sessionsError, setSessionsError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const [summaries, setSummaries] = useState<Map<string, ListSummary>>(
    new Map(),
  );
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);
  const [logSessionId, setLogSessionId] = useState<string | null>(null);
  const [shortcut] = useState(readShortcut);
  const [needsYouOnly, setNeedsYouOnly] = useState(shortcut.needsYouOnly);

  // Every navigation pushes a history entry (pushUrl), so Back has to read it
  // back. Without this the URL changed and the page did not — on a phone the
  // system back gesture left the open session on screen under a list URL.
  useEffect(() => {
    const onPop = () => {
      setView(readViewFromUrl());
      setSelectedId(readSessionIdFromUrl());
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

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
        setSessionsLoaded(true);
      })
      .catch((err: Error) => {
        if (++pollFailures.current >= 2) {
          setSessionsError(err.message);
          setSessionsLoaded(true);
        }
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

  useEffect(
    () => pollVisible(loadProposals, POLL_INTERVAL_MS),
    [loadProposals],
  );

  // Open decisions from the store, kept current by its watch stream. See
  // useDecisionDeck; the items it yields are derived below.
  const deck = useDecisionDeck();
  const { relist } = deck;
  // What an answer refreshes: the session poll for the row, the store for the
  // deck. Either alone leaves the answered decision up until the next tick.
  const reload = useCallback(() => {
    relist();
    return loadSessions();
  }, [relist, loadSessions]);

  // Sessions holding an unanswered QUESTION or a stamped PERMISSION, from the
  // summaries above. Both survive their pod (docs/adr/0050, 0082 §5) and
  // answering one warms a fresh pod to receive the answer, so these stay
  // answerable in the list whatever pod_phase says. An old worker's stranded
  // permission does not — see bucketSessions.
  //
  // Plus every session the deck can answer: a parked decision whose session
  // fell out of the summaries fetch is still answerable, and the list must not
  // say otherwise while the header counts it. Defined below, with the deck.
  const summaryAnswerableIds = useMemo(
    () =>
      new Set(
        [...summaries.entries()]
          .filter(([, sum]) => summaryAnswerable(sum))
          .map(([id]) => id),
      ),
    [summaries],
  );

  const needsYouIds = useMemo(
    () =>
      new Set(sessions.filter((t) => t.pendingDecisions > 0).map((t) => t.id)),
    [sessions],
  );

  // One transcript fetch per active session, feeding everything both list views
  // show beyond the Session row itself: the todo bar, the in-flight tool line, and
  // — the point of the rewrite — the actual pending decision, rendered inline so
  // a blocked session can be answered without opening it. Scoped to
  // LIVE_STATES, so it's bounded by the fleet's concurrency cap of 5, on the
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
    const active = sessions.filter((t) => LIVE_STATES.has(t.liveState));
    const strandedCap = 10;
    const stranded = sessions
      .filter(
        (t) =>
          !LIVE_STATES.has(t.liveState) &&
          t.pendingDecisions > 0 &&
          t.archivedAt === undefined,
      )
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
          // A withdrawn request leaves no transcript row (decisions.ts), so
          // the store is read too — only where a permission looks pending,
          // which keeps the usual poll at one fetch per session.
          .then(async (res) => {
            const entries =
              findPendingPermissions(res.entries).length > 0
                ? [...(await fetchWithdrawn(t.id)), ...res.entries]
                : res.entries;
            return [t.id, listSummary(entries)] as const;
          })
          .catch(() => null),
      ),
    ).then((results) => {
      if (cancelled) return;
      setSummaries(
        new Map(results.filter((r): r is NonNullable<typeof r> => r !== null)),
      );
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
    // Settings' own sub-page: leaving Settings must not carry it along.
    if (next !== "settings") url.searchParams.delete("section");
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
    // Every nav click lands on that view's top level. Leaving sessions drops
    // the open session (coming back to a stale ?session= would be surprising),
    // and clicking "sessions" while a session is open is the way back to the
    // list — it used to keep the session, so the already-highlighted tab did
    // nothing and the only way back was knowing the logo is a link.
    // Nothing to undo: re-clicking the tab you are on pushed a duplicate
    // history entry, and Back then appeared to do nothing.
    if (next === view && selectedId === null) return;
    setSelectedId(null);
    pushUrl(next, null);
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
  // The fallback deck, used only while the decisions store is unavailable
  // (see `items` below).
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

  // What the header counts, the sheet renders and needsYouOnly keeps: one
  // list, so the three cannot disagree. Open decisions the viewer can answer
  // (deckItems drops an old worker's request whose pod is gone), one item per
  // decision. Without the store (FLEET_AUTH_DISABLED=1, where AgentService
  // refuses every call) it is today's derivation: one item per blocked
  // session, from the summaries.
  const items = useMemo<DeckItem[]>(
    () =>
      deck.available
        ? deckItems(deck.open, sessions)
        : blockedSessions.map((s) => ({
            key: s.id,
            session: s,
            summary: summaries.get(s.id),
          })),
    [deck.available, deck.open, sessions, blockedSessions, summaries],
  );
  const itemSessionIds = useMemo(
    () => new Set(items.map((i) => i.session.id)),
    [items],
  );
  const answerableIds = useMemo(
    () =>
      new Set([
        ...summaryAnswerableIds,
        ...items.filter((i) => i.decision).map((i) => i.session.id),
      ]),
    [summaryAnswerableIds, items],
  );

  const counts = useMemo(() => {
    // The deck, not pendingDecisions: that raw count includes sessions whose
    // pod is gone (and archived ones), so the badge would show work waiting on
    // a human that no human can act on. Decisions, not sessions: a session
    // holding two counts two.
    const waiting = items.length;
    // `working` means the badge says WORKING, not "has a live pod".
    // LIVE_STATES is the pod-liveness set — it includes idle, unknown and
    // stalled — so counting it here reported "1 working · 0 idle" for a fleet
    // whose only live session was rendering an IDLE badge two lines below.
    // The header and the row have to agree; they are read together.
    const working = sessions.filter((t) => t.liveState === "working").length;
    const open = (t: Session) =>
      t.archivedAt === undefined && t.sweptAt === undefined;
    const done = sessions.filter(
      (t) => open(t) && t.liveState === "done",
    ).length;
    // Idle is the QUIET tail's own default — not archived, not swept. It was
    // "everything else", so the header said 21 idle over a list showing 4.
    // A session with no pod has no liveState at all (""), and is the most
    // common idle session there is — so this excludes the active states
    // rather than matching "idle".
    const idle = sessions.filter(
      (t) =>
        t.archivedAt === undefined &&
        t.sweptAt === undefined &&
        !["working", "done", "blocked"].includes(t.liveState),
    ).length;
    return { waiting, working, done, idle };
  }, [sessions, items]);

  const filteredSessions = useMemo(() => {
    // needsYouOnly is the manifest's "Waiting on you" shortcut. Mobile already
    // opens on its needs-you bucket, so this only changes the desktop list —
    // but it narrows the shared array either way, which keeps the two honest.
    //
    // Same predicate as the header count, and for the same reason:
    // pendingDecisions is a raw count that stays above zero forever when a pod
    // dies mid-decision, so filtering on it showed sessions nobody can act on
    // — and hid nothing, since the shortcut's whole promise is "these are
    // waiting on you".
    // Text search is server-side now (the poll passes `query`), so this only
    // applies the needsYouOnly live-state shortcut — a predicate the server
    // doesn't know about.
    return needsYouOnly
      ? sessions.filter((t) => itemSessionIds.has(t.id))
      : sessions;
  }, [sessions, needsYouOnly, itemSessionIds]);

  const shared = {
    answerableIds,
    sessions: filteredSessions,
    summaries,
    needsYouIds,
    onSelect: selectSession,
    onDelete: deleteSession,
    onOpenLogs: setLogSessionId,
    reload,
    loaded: sessionsLoaded,
  };

  return (
    // h-dvh, not h-screen: 100vh is pinned to the largest possible viewport on
    // mobile browsers, so it doesn't shrink when the keyboard opens and the
    // composer ends up underneath it. h-dvh tracks the visible viewport.
    <div className="h-dvh overflow-hidden bg-base-100 text-base-content flex flex-col">
      {isDesktop ? (
        <div className="flex-none flex items-center gap-4 px-4.5 py-3 border-b border-line bg-base-200">
          <a
            href="/"
            className="flex items-center gap-2 text-lg font-semibold tracking-[0.02em] text-primary hover:opacity-80"
          >
            <Logo size={18} />
            herd
          </a>
          {/* No repo count: it was the distinct repos among the loaded (and
              search-filtered) sessions, shown beside a picker listing more. */}
          <span className="text-xs text-dim2 whitespace-nowrap">
            ukubi-cluster
          </span>
          <Segmented
            value={view}
            options={withProposalCount(NAV, proposals.length)}
            onChange={selectView}
            className="ml-1"
          />

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
                  {counts.waiting} decision{counts.waiting === 1 ? "" : "s"}{" "}
                  waiting on you
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
          {/* The one header item allowed to give way: everything else is a
              control, and at laptop widths "N waiting on you" pushed
              "+ new session" off the right edge. */}
          <span
            className={`text-xs text-dim2 whitespace-nowrap min-w-0 truncate ${counts.waiting > 0 ? "" : "ml-auto"}`}
          >
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
          <SettingsButton
            active={view === "settings"}
            onClick={() => selectView("settings")}
          />
        </div>
      ) : (
        <div className="flex-none border-b border-line bg-base-200">
          <div className="flex items-center gap-2.5 px-3.5 py-2.5">
            <a
              href="/"
              className="flex items-center gap-1.5 text-lg font-semibold text-primary"
            >
              <Logo size={16} />
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
                  {counts.waiting} decision{counts.waiting === 1 ? "" : "s"}
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
            <SettingsButton
              active={view === "settings"}
              onClick={() => selectView("settings")}
            />
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

      <ErrorModal
        message={sessionsError}
        onClose={() => setSessionsError(null)}
      />
      <ConfirmModal
        open={pendingDeleteId !== null}
        message="Delete this session? This tears down any live pod and removes it from the list."
        onConfirm={confirmDeleteSession}
        onCancel={() => setPendingDeleteId(null)}
      />
      <LogDrawer
        sessionId={logSessionId}
        onClose={() => setLogSessionId(null)}
      />
      <NeedsYouModal
        open={decisionsOpen}
        items={items}
        available={deck.available}
        onClose={() => setDecisionsOpen(false)}
        onOpenSession={(id) => {
          // Close first: a plan sends you into the session, and leaving this
          // open behind the detail view means answering there and then finding
          // a stale copy of the same decision still sitting on top.
          setDecisionsOpen(false);
          selectSession(id);
        }}
        reload={reload}
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
        {view === "settings" ? (
          <Settings
            theme={theme}
            onThemeChange={setTheme}
            feedWidth={feedWidth}
            onFeedWidthChange={setFeedWidth}
          />
        ) : view === "files" ? (
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
            <SessionDetail sessionId={selectedId} sessions={sessions} />
          ) : (
            <MobileSessionDetail
              sessionId={selectedId}
              sessions={sessions}
              onBack={clearSelection}
              onDelete={() => deleteSession(selectedId)}
            />
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
          options={withProposalCount(MOBILE_NAV, proposals.length)}
          onChange={selectView}
          grow
          size="lg"
          className="flex-none border-x-0 border-b-0 border-t"
        />
      )}
    </div>
  );
}
