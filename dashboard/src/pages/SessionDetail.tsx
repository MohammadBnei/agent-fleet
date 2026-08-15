import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { sessionBadge } from "./SessionList";
import {
  feedVisibility,
  latestResultSummary,
  latestSlashCommands,
  latestToolCallSummary,
  latestTodos,
  type Density,
} from "../transcript";
import { useSessionDetail } from "../useSessionDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { ErrorModal } from "../components/ErrorModal";
import { Markdown } from "../components/Markdown";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Segmented } from "../components/Segmented";
import { DecisionDock } from "../components/DecisionDock";
import { SessionFeed } from "../components/SessionFeed";
import { TodosPanel, ChangesPanel, SessionPanel } from "../components/SessionPanels";
import { asDisplayMarkdown } from "../transcript";

// The console's desktop session view: feed · decision dock · composer, beside a
// fixed panel column. Full-width — the permanent 320px session-list sidebar is
// gone, and the rich list view is the fleet overview it used to stand in for.
//
// The pending decision lives in the dock and nowhere else (docs/adr/0043): it
// used to render inline in the feed *and* as a spine item *and* as an answer
// chip above the composer. `dockPendingDecision` is what keeps the feed from
// rendering it a second time.

const DENSITY: readonly { value: Density; label: string; title: string }[] = [
  { value: "everything", label: "everything", title: "every entry, including tool calls and lifecycle lines" },
  { value: "narrative", label: "narrative", title: "the readable conversation only" },
  { value: "decisions", label: "decisions", title: "decisions and alarms only" },
];

export function SessionDetail({
  sessionId,
  sessions,
  onBack,
  onClosed: _onClosed,
}: {
  sessionId: string;
  sessions: Session[];
  onBack: () => void;
  // Called when this session stops existing (dismissing a proposal soft-deletes
  // it), so the view doesn't sit on a row that is no longer there.
  onClosed?: () => void;
}) {
  const {
    session: fetchedSession,
    entries,
    branch,
    worktreePath,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    respondToPermission,
    answerQuestion,
    clearActionError,
  } = useSessionDetail(sessionId);
  const [message, setMessage] = useState("");
  const [bypassOpen, setBypassOpen] = useState(false);
  // Key deliberately not renamed with the file: it is persisted in every
  // operator's browser, and renaming it silently resets their density choice.
  const [density, setDensity] = useLocalStorageState<Density>("taskDetail.density", "everything");
  const { ref: feedRef, atBottom: feedAtBottom, onScroll: feedOnScroll, scrollToBottom: feedScrollToBottom } =
    useAtBottom<HTMLDivElement>();

  // A new human entry (this tab's own send, or one relayed from Discord) always
  // jumps to the bottom — the reader clearly wants to see it. A new agent entry
  // follows too, but only if the reader was already at the bottom, otherwise
  // they're mid-scroll reading history and it'd yank the view; the button
  // pulses instead.
  const [hasNewAiMessage, setHasNewAiMessage] = useState(false);
  const prevEntriesLenRef = useRef<number | null>(null);
  useEffect(() => {
    if (prevEntriesLenRef.current === null) {
      prevEntriesLenRef.current = entries.length;
      return;
    }
    if (entries.length > prevEntriesLenRef.current) {
      const latest = entries[entries.length - 1];
      if (latest.from === "human" || feedAtBottom) {
        feedScrollToBottom();
        setHasNewAiMessage(false);
      } else {
        setHasNewAiMessage(true);
      }
    }
    prevEntriesLenRef.current = entries.length;
  }, [entries, feedAtBottom, feedScrollToBottom]);

  // Land on the latest message when a session is opened — *before* the first
  // paint of that session's feed, not after it. As a plain effect this ran
  // post-paint, so opening a session showed the top of the history for a
  // frame and then visibly scrolled down; useLayoutEffect means you simply
  // arrive at the bottom. This component instance is reused across session
  // switches (App.tsx doesn't key it by id), so track which session we've
  // already jumped for instead of scrolling every render.
  const scrolledForSessionRef = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (entries.length === 0 || scrolledForSessionRef.current === sessionId) return;
    scrolledForSessionRef.current = sessionId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [sessionId, entries, feedRef]);

  // The optimistic echo of a just-sent message (useSessionDetail's
  // pendingMessage) renders immediately but below the fold. Only the entries
  // effect above scrolled, and that waits for the real entry to come back
  // over the stream — so a send appeared to do nothing until the round trip
  // finished, and then jumped.
  useEffect(() => {
    if (pendingMessage !== null) feedScrollToBottom();
  }, [pendingMessage, feedScrollToBottom]);
  useEffect(() => {
    if (feedAtBottom) setHasNewAiMessage(false);
  }, [feedAtBottom]);

  if (loadError) return <div className="m-4 border border-pink-line bg-pink-bg px-4 py-3 text-base text-error">{loadError}</div>;
  if (!fetchedSession) return <div className="p-4 text-base text-dim">Loading…</div>;

  // `sessions` is App.tsx's already-polled (5s) list — preferring it keeps
  // pod_phase/status live without a second poll loop just for this page; falls
  // back to the one-shot fetch for the instant before the next tick includes it.
  const session = sessions.find((t) => t.id === sessionId) ?? fetchedSession;

  const badge = sessionBadge(session);
  // staleTag/heartbeat used to render "last beat 4m ago", reddened past the
  // reclaim threshold. Both are gone with heartbeat_at (docs/adr/0048) — the
  // feed's own entry timestamps say when this session last did anything, and
  // they say it about real work rather than about a timer.
  const blocked = session.liveState === "blocked";
  const visibility = feedVisibility(density, false);

  const todos = latestTodos(entries) ?? [];
  const changes = latestToolCallSummary(entries)?.files ?? null;
  const result = latestResultSummary(entries);
  const contextTokens =
    (result?.usage?.input_tokens ?? 0) +
    (result?.usage?.cache_read_input_tokens ?? 0) +
    (result?.usage?.cache_creation_input_tokens ?? 0);

  // Lean name-only autocomplete (the SDK's init message carries no descriptions
  // or argument hints at runtime, docs/adr/0027) — only while the draft looks
  // like a command, filtered by what's typed so far.
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
    <div className="flex-1 min-h-0 flex">
      <div className="flex-1 min-w-0 flex flex-col border-r border-line">
        <div className="flex-none flex items-center gap-3 px-4.5 py-3 border-b border-line flex-wrap">
          <button type="button" onClick={onBack} className="text-sm text-dim hover:text-primary cursor-pointer">
            ← all sessions
          </button>
          <h2 className="text-lg font-semibold min-w-0 break-words">
            #{session.id.slice(0, 6)} {sessionLabel(session)}
          </h2>
          {blocked ? (
            <span className="flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
              <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              <span className="text-xs font-medium text-error">blocked</span>
            </span>
          ) : (
            badge && (
              <span
                className={`text-xs px-1.5 py-px border tracking-wide flex-none ${badge.className}`}
                title={badge.title ?? session.podMessage ?? undefined}
              >
                {badge.label}
              </span>
            )
          )}
          
          <span className="text-xs text-dim2 min-w-0 truncate">
            {session.repo}
            {branch && ` · ${branch}`}
          </span>
          
          <div className="ml-auto flex items-center gap-2 flex-none">
            <span className="text-2xs tracking-[0.1em] text-dim2">DENSITY</span>
            <Segmented value={density} options={DENSITY} onChange={setDensity} />
          </div>
        </div>

        <div className="relative flex-1 min-h-0">
          <div
            ref={feedRef}
            onScroll={feedOnScroll}
            className="absolute inset-0 overflow-y-auto px-4.5 py-4.5 flex flex-col gap-4"
          >
            <SessionFeed
              entries={entries}
              visibility={visibility}
              density={density}
              busyKey={busyKey}
              dockPendingDecision
              onRespond={respondToPermission}
              onAnswer={answerQuestion}
              onPlanFeedback={(text) => sendDiscuss(text)}
            />
            {pendingMessage && (
              <div className="flex gap-2.5 items-baseline opacity-60">
                <span className="text-primary flex-none text-base">❯</span>
                <div className="flex-1 min-w-0 text-base leading-[1.7] text-text2">
                  <Markdown text={asDisplayMarkdown({ from: "human", text: pendingMessage })} />
                </div>
                <span className="loading loading-spinner loading-xs flex-none" />
              </div>
            )}
          </div>
          {!feedAtBottom && (
            <button
              type="button"
              onClick={feedScrollToBottom}
              className="absolute bottom-3 left-1/2 -translate-x-1/2 border border-line bg-base-300 px-2 py-1 text-sm shadow-md"
              title="Scroll to bottom"
            >
              ↓
              {hasNewAiMessage && (
                <span className="absolute -top-1 -right-1 w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              )}
            </button>
          )}
        </div>

        <DecisionDock
          entries={entries}
          busyKey={busyKey}
          onRespond={respondToPermission}
          onAnswer={answerQuestion}
          onPlanFeedback={(text) => sendDiscuss(text)}
        />

        <div className="flex-none px-4.5 py-3 border-t border-line">
          <ErrorModal message={actionError} onClose={clearActionError} />
          <BypassConfirmModal
            open={bypassOpen}
            onCancel={() => setBypassOpen(false)}
            onConfirm={() => {
              setBypassOpen(false);
              run(() => client.setPermissionMode({ sessionId, mode: "bypassPermissions" }), "bypass");
            }}
          />

          {slashCommands.length > 0 && (
            <div className="flex gap-2 mb-2.5 flex-wrap">
              {slashCommands.map((c) => (
                <button
                  key={c}
                  type="button"
                  onClick={() => setMessage(`/${c} `)}
                  className="border border-line text-dim px-3 py-1 text-xs cursor-pointer hover:text-base-content"
                >
                  /{c}
                </button>
              ))}
            </div>
          )}

          <div className="flex gap-3 items-center border border-line px-3 py-2.5 focus-within:border-primary/60">
            <span className="text-primary text-base">❯</span>
            <input
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendMessage();
              }}
              disabled={busyKey !== null}
              placeholder="message the agent — / for commands"
              aria-label="message the agent"
              className="flex-1 min-w-0 bg-transparent outline-none text-base placeholder:text-dim2"
            />
            <button
              type="button"
              disabled={busyKey !== null || !message.trim()}
              onClick={sendMessage}
              className="text-xs text-dim2 hover:text-primary disabled:opacity-40 disabled:hover:text-dim2 cursor-pointer flex-none"
            >
              ⏎ send
            </button>
          </div>

          <div className="flex gap-3.5 mt-2 text-xs text-dim2 flex-wrap">
            {worktreePath && <span className="truncate max-w-[320px]" title={worktreePath}>{worktreePath}</span>}
            {branch && <span>{branch}</span>}
            {result && (
              // Per-turn, not cumulative: the SDK reports usage per result and
              // nothing sums it, so labelling this a session total would be a
              // lie. See docs/dashboard-spec.md — no cost/token rollup exists.
              <span title="context used by the most recent turn — there is no session-level rollup on the wire">
                ctx {Math.round(contextTokens / 1000)}k last turn
              </span>
            )}
            <span className={session.permissionMode === "bypassPermissions" ? "text-warning" : undefined}>
              ▸▸ {session.permissionMode || "default"} permissions
            </span>
          </div>
        </div>
      </div>

      <div className="w-[266px] flex-none overflow-y-auto px-3.5 py-3.5 flex flex-col gap-4.5 min-w-0">
        <TodosPanel todos={todos} blocked={blocked} />
        <ChangesPanel branch={branch} changes={changes} />
        <div className="mt-auto">
          <SessionPanel
            session={session}
            busy={busyKey !== null}
            busyKey={busyKey}
            run={run}
            onBypassClick={() => setBypassOpen(true)}
            onRestart={() => sendDiscuss("/clear")}
          />
        </div>
      </div>

    </div>
  );
}
