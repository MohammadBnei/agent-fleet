import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import {
  feedVisibility,
  latestSlashCommands,
  latestToolCallSummary,
  latestTodos,
  subagentRuns,
  backgroundTasks,
  type Density,
  hasPendingDecision,
} from "../transcript";
import { useSessionDetail } from "../useSessionDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { sessionBadge } from "../pages/SessionList";
import { ActionButton } from "../components/ActionButton";
import { DecisionDock } from "../components/DecisionDock";
import { ErrorModal } from "../components/ErrorModal";
import { ConfirmModal } from "../components/ConfirmModal";
import { AUTO_MODE_WARNING } from "../approvePlan";
import { Modal } from "../components/Modal";
import { Segmented } from "../components/Segmented";
import { SessionFeed } from "../components/SessionFeed";
import { TodosPanel, ChangesPanel, AgentsPanel, BackgroundPanel, SessionPanel } from "../components/SessionPanels";
import { FileDiffModal } from "../components/FileDiffModal";
import { isPodPhaseLive } from "../pages/SessionList";
import { AgentDetailModal } from "../components/DetailModal";
import type { SubagentRun } from "../transcript";
import { Composer } from "../components/Composer";

// The phone session screen from Agent Fleet Console Mobile.dc.html.
//
// The structural difference from desktop is the **decision dock**: when the
// agent is waiting on a human, its request pins to the bottom above the
// composer instead of sitting inline in the feed. On a phone an inline card
// scrolls away, and this is the surface most likely used to unblock a session
// away from a desk (docs/dashboard-spec.md §8 items 3 and 5).

const DENSITY: readonly { value: Density; label: string; title: string }[] = [
  { value: "everything", label: "all", title: "everything" },
  { value: "narrative", label: "talk", title: "the readable conversation only" },
  { value: "decisions", label: "calls", title: "tool activity and decisions" },
];

export function MobileSessionDetail({
  sessionId,
  onBack,
  onDelete,
}: {
  sessionId: string;
  onBack: () => void;
  onDelete: () => void;
}) {
  const {
    session,
    entries,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    hasOlder,
    loadingOlder,
    loadOlder,
    run,
    sendDiscuss,
    respondToPermission,
    approvePlanDecision,
    answerQuestion,
    clearActionError,
  } = useSessionDetail(sessionId);
  const [message, setMessage] = useState("");
  const [panelsOpen, setPanelsOpen] = useState(false);
  // The title sheet: the full label, which the header can only ever truncate,
  // plus the density control it used to compete with for the same row.
  const [titleOpen, setTitleOpen] = useState(false);
  const [autoOpen, setAutoOpen] = useState(false);
  // The CHANGES row a human clicked, or null. See FileDiffModal.
  const [diffPath, setDiffPath] = useState<string | null>(null);
  // The AGENTS row a human tapped, or null. See DetailModal.
  const [agentDetail, setAgentDetail] = useState<SubagentRun | null>(null);
  // Same persisted key as the desktop view — see SessionDetail.tsx.
  const [density, setDensity] = useLocalStorageState<Density>("taskDetail.density", "everything");
  const { ref: feedRef, atBottom, onScroll, scrollToBottom, anchorPrepend } = useAtBottom<HTMLDivElement>();

  // Keyed on the newest seq rather than the count — a "load earlier" prepend
  // grows the list without anything having arrived. Same reasoning as
  // SessionDetail's copy.
  const prevLastSeq = useRef<bigint | null>(null);
  useEffect(() => {
    const latest = entries[entries.length - 1];
    const prev = prevLastSeq.current;
    prevLastSeq.current = latest?.seq ?? null;
    if (prev === null || latest === undefined || latest.seq <= prev) return;
    if (latest.from === "human" || atBottom) scrollToBottom();
  }, [entries, atBottom, scrollToBottom]);

  // Before the first paint of this session's feed, not after — as a plain effect
  // it painted the top of the history and then visibly scrolled down. Same
  // reasoning as SessionDetail's copy.
  const scrolledFor = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (entries.length === 0 || scrolledFor.current === sessionId) return;
    scrolledFor.current = sessionId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [sessionId, entries, feedRef]);

  // The optimistic echo renders before the server has the message; without
  // this, a send only scrolled once the real entry came back over the stream.
  useEffect(() => {
    if (pendingMessage !== null) scrollToBottom();
  }, [pendingMessage, scrollToBottom]);

  if (loadError) {
    return (
      <div className="flex-1 min-h-0 p-3.5">
        <button type="button" onClick={onBack} className="text-base text-dim mb-3">
          ←
        </button>
        <div className="border border-pink-line bg-pink-bg px-3 py-2.5 text-sm text-error">{loadError}</div>
      </div>
    );
  }
  if (!session) return <div className="flex-1 p-4 text-base text-dim">Loading…</div>;

  const blocked = session.liveState === "blocked";
  const badge = sessionBadge(session);
  const visibility = feedVisibility(density, true);
  const todos = latestTodos(entries) ?? [];
  // One lookup for both: the sidecar pushes branch and files in the same
  // telemetry snapshot. `branch` used to come from useSessionDetail, where it
  // was permanently null — the ListWorktrees lookup that filled it died with
  // the worktree model (docs/adr/0048 §5) while the real value was on the wire
  // here the whole time, already rendered by ToolCallLine in the feed.
  const toolSummary = latestToolCallSummary(entries);
  const branch = toolSummary?.branch ?? null;
  const changes = toolSummary?.files ?? null;
  const agents = subagentRuns(entries);
  const background = backgroundTasks(entries);

  const docked = hasPendingDecision(entries);

  const slashCommands = message.startsWith("/")
    ? (latestSlashCommands(entries) ?? []).filter((c) => c.toLowerCase().startsWith(message.slice(1).toLowerCase()))
    : [];

  function sendMessage() {
    const text = message.trim();
    if (!text) return;
    setMessage("");
    sendDiscuss(text);
  }

  return (
    <div className="flex-1 min-h-0 flex flex-col">
      {/* Three rows on a 390px screen, and the title lost every fight: it was
          truncated to about two words by a status chip on its right and a
          three-cell density control stealing the row below. Tapping the title
          now opens both the full text and the density choice, which takes the
          Segmented out of the header entirely and gives the remaining rows room
          to breathe. */}
      <div className="flex-none px-4 py-3.5 border-b border-line">
        {/* No back arrow and no chevron. Back is the header's "herd" logo, which
            is a real <a href="/"> — a full page load that resets the SPA to the
            list — so the row does not need its own. onBack survives for the
            load-error screen above, which has no header to fall back on. */}
        <div className="flex items-center">
          <button
            type="button"
            onClick={() => setTitleOpen(true)}
            aria-label="Session title and feed density"
            className="min-w-0 flex-1 text-left cursor-pointer"
          >
            {/* No id. It is a six-character hex string that means nothing to
                read and was taking the front of every title; it lives in the
                SESSION panel now, where the other facts are. */}
            <span className="text-base font-semibold min-w-0 truncate block">
              {sessionLabel(session)}
            </span>
          </button>
        </div>

        {/* Repo, status and the panels opener share one line. The badge moved
            off the title row so the title gets the full width, and the todo bar
            that used to own a third row is gone from the header entirely — the
            panels sheet already renders it in full, with the actual todo list
            under it, so the header copy was a duplicate that cost a row. */}
        <div className="flex items-center gap-2.5 mt-3">
          {/* The repo sizes to its content rather than taking flex-1, capped so
              a long repo·branch cannot crowd the rest out. That leaves the
              middle free to grow, which is what centres the badge in the gap
              between the repo and the panels button instead of parking it
              against the button. */}
          <span className="text-2xs text-dim2 min-w-0 truncate flex-none max-w-[55%]">
            {session.repo}
            {branch && ` · ${branch}`}
          </span>
          <div className="flex-1 min-w-0 flex justify-center">
            {blocked ? (
              <span className="flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
                <span className="w-[5px] h-[5px] rounded-full bg-error animate-fpulse" />
                <span className="text-xs font-medium text-error">blocked</span>
              </span>
            ) : (
              badge && (
                <span className={`flex-none text-2xs px-1 border tracking-wide ${badge.className}`}>
                  {badge.label}
                </span>
              )
            )}
          </div>
          {/* ALWAYS rendered. This was briefly gated on the three conditional
              panels all being empty, which was wrong twice over: the sheet also
              carries the SESSION panel — model/mode/pod plus the entire
              ActionsMenu and mobile's only Delete — so it is never actually
              empty, and a session with no todos and no file changes lost every
              action it had. Worse, the gate was computed from live state, so
              the button appeared the moment the agent wrote a todo and vanished
              again afterwards: a control that comes and goes while you watch. */}
          <button
            type="button"
            onClick={() => setPanelsOpen(true)}
            aria-label="Open panels"
            title="Todos, changes, agents and session actions"
            // A bordered box, not the bare "panels ▸" text it replaces: at
            // text-2xs that label was a ~40x12px tap target on the busiest edge
            // of the screen, next to the status badge. This is square and
            // finger-sized, and the border is what makes it read as a control
            // rather than a third piece of status text on a row that already
            // has two.
            className="flex-none flex items-center justify-center w-8 h-8 border border-line text-dim hover:text-base-content hover:border-acc-line cursor-pointer"
          >
            <span className="text-sm leading-none">▤</span>
          </button>
        </div>
      </div>

      <div ref={feedRef} onScroll={onScroll} className="flex-1 min-h-0 overflow-y-auto px-3.5 py-3.5 flex flex-col gap-3.5">
        <SessionFeed
          entries={entries}
          visibility={visibility}
          density={density}
          busyKey={busyKey}
          hasOlder={hasOlder}
          loadingOlder={loadingOlder}
          onLoadOlder={() => anchorPrepend(loadOlder)}
          compact
          dockPendingDecision
          onRespond={respondToPermission}
          onApprovePlan={approvePlanDecision}
          onAnswer={answerQuestion}
          onPlanFeedback={(text) => sendDiscuss(text)}
        />
        {pendingMessage && (
          <div className="flex gap-2.5 items-baseline opacity-60">
            <span className="text-primary flex-none text-sm">❯</span>
            <div className="text-sm leading-[1.7] text-text2 min-w-0 flex-1">{pendingMessage}</div>
            <span className="loading loading-spinner loading-xs flex-none" />
          </div>
        )}
      </div>

      <ErrorModal message={actionError} onClose={clearActionError} />
      {/* A sibling of the panels sheet, not a child: the sheet is where the
          CHANGES row is clicked, and a <dialog> nested inside an open one is
          the trap Modal.tsx:36 guards. Opening the diff closes the sheet
          (setPanelsOpen(false) below) so there is only ever one open. */}
      <FileDiffModal sessionId={sessionId} path={diffPath} entries={entries} onClose={() => setDiffPath(null)} />
      <AgentDetailModal run={agentDetail} onClose={() => setAgentDetail(null)} />
      <ConfirmModal
        title="Switch to auto mode?"
        message={AUTO_MODE_WARNING}
        confirmLabel="switch to auto"
        danger={false}
        open={autoOpen}
        onCancel={() => setAutoOpen(false)}
        onConfirm={() => {
          setAutoOpen(false);
          run(() => client.setPermissionMode({ sessionId, mode: "auto" }), "auto");
        }}
      />

      <DecisionDock
        entries={entries}
        podLive={isPodPhaseLive(session.podPhase)}
        swept={session.sweptAt !== undefined}
        busyKey={busyKey}
        compact
        onRespond={respondToPermission}
        onApprovePlan={approvePlanDecision}
        onAnswer={answerQuestion}
        onPlanFeedback={(text) => sendDiscuss(text)}
      />

      <div className="flex-none border-t border-line">
        {(docked || slashCommands.length > 0) && (
          <div className="flex gap-2 px-3.5 pt-2.5 overflow-x-auto">
            <ActionButton
              busy={busyKey === "action:interrupt"}
              disabled={busyKey !== null}
              onClick={() => run(() => client.interrupt({ sessionId }), "action:interrupt")}
              className="flex-none border border-line text-dim px-2.5 py-1.5 text-xs whitespace-nowrap"
            >
              /interrupt
            </ActionButton>
            <ActionButton
              busy={busyKey === "action:mode"}
              disabled={busyKey !== null}
              onClick={() => run(() => client.setPermissionMode({ sessionId, mode: "plan" }), "action:mode")}
              className="flex-none border border-line text-dim px-2.5 py-1.5 text-xs whitespace-nowrap"
            >
              /mode plan
            </ActionButton>
            {slashCommands.map((c) => (
              <button
                key={c}
                type="button"
                onClick={() => setMessage(`/${c} `)}
                className="flex-none border border-line text-dim px-2.5 py-1.5 text-xs whitespace-nowrap"
              >
                /{c}
              </button>
            ))}
          </div>
        )}

        <div className="px-3.5 py-2.5">
          <Composer
            value={message}
            onChange={setMessage}
            onSend={sendMessage}
            disabled={busyKey !== null}
            placeholder="message the agent"
            compact
          />
        </div>
      </div>

      {/* What the header cannot show: the full title, untruncated, and the feed
          density that used to eat a third of the row below it. */}
      <Modal open={titleOpen} onClose={() => setTitleOpen(false)}>
        <div className="flex flex-col gap-5">
          <div className="flex flex-col gap-1.5">
            <span className="text-2xs tracking-[0.12em] text-dim2">SESSION</span>
            {/* break-words, not truncate — the entire point of this sheet is
                that the header already truncated it. */}
            <span className="text-base font-semibold break-words">{sessionLabel(session)}</span>
            <span className="text-2xs text-dim2 break-all">
              #{session.id.slice(0, 6)} · {session.repo}
              {branch && ` · ${branch}`}
            </span>
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-2xs tracking-[0.12em] text-dim2">DENSITY</span>
            <Segmented value={density} options={DENSITY} onChange={setDensity} grow size="lg" />
          </div>
        </div>
      </Modal>

      {/* Everything the desktop right column carries, as a bottom sheet. */}
      {/* max-h + an inner scroller, the pattern LogDrawer already uses. Without
          it the sheet relied on daisyUI's modal-box default and a long todo
          list plus a handful of subagents ran straight past the viewport with
          nothing to grab. min-h-0 is load-bearing: a flex child will not scroll
          without it, the same reason the desktop column's row needs it. */}
      <Modal
        open={panelsOpen}
        onClose={() => setPanelsOpen(false)}
        boxClassName="max-h-[85vh] overflow-hidden flex flex-col"
      >
        <div className="flex flex-col gap-5 overflow-y-auto min-h-0">
          <TodosPanel todos={todos} blocked={blocked} />
          <ChangesPanel branch={branch} changes={changes} onOpenFile={(path) => {
            setPanelsOpen(false);
            setDiffPath(path);
          }} />
          <AgentsPanel
            runs={agents}
            onOpenAgent={(run) => {
              // Close the sheet first: a <dialog> opened inside an open one
              // is the nesting Modal.tsx guards, and the sheet is not worth
              // keeping behind a detail you opened from it.
              setPanelsOpen(false);
              setAgentDetail(run);
            }}
          />
          <BackgroundPanel tasks={background} />
          <SessionPanel
            session={session}
            busy={busyKey !== null}
            busyKey={busyKey}
            run={run}
            onAutoClick={() => {
              setPanelsOpen(false);
              setAutoOpen(true);
            }}
            onRestart={() => {
              setPanelsOpen(false);
              sendDiscuss("/clear");
            }}
          />
          <button
            type="button"
            onClick={onDelete}
            className="border border-pink-line text-error px-3 py-2.5 text-sm self-start"
          >
            Delete session
          </button>
        </div>
      </Modal>
    </div>
  );
}
