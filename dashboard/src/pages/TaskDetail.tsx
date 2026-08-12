import { useCallback, useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { isThot, repoLabel } from "../taskKind";
import type { Task } from "../gen/agentfleet/v1/core_pb";
import { sessionBadge, staleBadge, heartbeatLabel, prBadge } from "./TaskList";
import {
  feedVisibility,
  findPendingQuestion,
  latestResultSummary,
  latestSlashCommands,
  latestToolCallSummary,
  latestTodos,
  parseQuestions,
  spineItems,
  type Density,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { ErrorModal } from "../components/ErrorModal";
import { Markdown } from "../components/Markdown";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Segmented } from "../components/Segmented";
import { DecisionSpine } from "../components/DecisionSpine";
import { DecisionInline } from "../components/DecisionInline";
import { NotchCard } from "../components/NotchCard";
import { SessionFeed } from "../components/SessionFeed";
import { TodosPanel, ChangesPanel, E2ePanel, SessionPanel } from "../components/SessionPanels";
import { asDisplayMarkdown } from "../transcript";

// The console's desktop session view: decision-spine rail · feed · fixed panel
// column. Full-width — the permanent 320px task-list sidebar is gone, and the
// rich list view is the fleet overview it used to stand in for.

const DENSITY: readonly { value: Density; label: string; title: string }[] = [
  { value: "everything", label: "everything", title: "every entry, including tool calls and lifecycle lines" },
  { value: "narrative", label: "narrative", title: "the readable conversation only" },
  { value: "decisions", label: "decisions", title: "decisions and alarms only" },
];

export function TaskDetail({
  taskId,
  tasks,
  onBack,
  onClosed,
}: {
  taskId: string;
  tasks: Task[];
  onBack: () => void;
  // Called when this task stops existing (dismissing a proposal soft-deletes
  // it), so the view doesn't sit on a row that is no longer there.
  onClosed?: () => void;
}) {
  const {
    task: fetchedTask,
    entries,
    previewUrl,
    e2e,
    branch,
    worktreePath,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    clearActionError,
  } = useTaskDetail(taskId);
  const [message, setMessage] = useState("");
  const [bypassOpen, setBypassOpen] = useState(false);
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

  // Land on the latest message when a task is opened. This component instance
  // is reused across task switches (App.tsx doesn't key it by id), so track
  // which task we've already jumped for instead of scrolling every render.
  const scrolledForTaskRef = useRef<string | null>(null);
  useEffect(() => {
    if (entries.length === 0 || scrolledForTaskRef.current === taskId) return;
    scrolledForTaskRef.current = taskId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [taskId, entries, feedRef]);
  useEffect(() => {
    if (feedAtBottom) setHasNewAiMessage(false);
  }, [feedAtBottom]);

  // Spine → feed. The feed tags each row with the entry seq, so this lands on
  // the real entry rather than an estimated offset.
  const jumpToEntry = useCallback((seq: bigint) => {
    const el = document.getElementById(`entry-${seq}`);
    if (el) el.scrollIntoView({ behavior: "smooth", block: "center" });
  }, []);

  if (loadError) return <div className="m-4 border border-pink-line bg-pink-bg px-4 py-3 text-[13px] text-error">{loadError}</div>;
  if (!fetchedTask) return <div className="p-4 text-[13px] text-dim">Loading…</div>;

  // `tasks` is App.tsx's already-polled (5s) list — preferring it keeps
  // pod_phase/status live without a second poll loop just for this page; falls
  // back to the one-shot fetch for the instant before the next tick includes it.
  const task = tasks.find((t) => t.id === taskId) ?? fetchedTask;

  const badge = sessionBadge(task);
  const staleTag = staleBadge(task);
  const heartbeat = heartbeatLabel(task);
  const prLink = prBadge(task);
  const blocked = task.liveState === "blocked";
  const visibility = feedVisibility(density, false);
  const items = spineItems(entries);
  const firstPending = items.find((i) => i.kind === "pending") ?? null;

  const todos = latestTodos(entries) ?? [];
  const changes = latestToolCallSummary(entries)?.files ?? null;
  const result = latestResultSummary(entries);
  const contextTokens =
    (result?.usage?.input_tokens ?? 0) +
    (result?.usage?.cache_read_input_tokens ?? 0) +
    (result?.usage?.cache_creation_input_tokens ?? 0);

  const pendingQuestion = findPendingQuestion(entries);
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;
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
      <DecisionSpine
        items={items}
        onJump={jumpToEntry}
        onNextDecision={firstPending ? () => jumpToEntry(firstPending.seq) : null}
      />

      <div className="flex-1 min-w-0 flex flex-col border-r border-line">
        <div className="flex-none flex items-center gap-3 px-4.5 py-3 border-b border-line flex-wrap">
          <button type="button" onClick={onBack} className="text-[12.5px] text-dim hover:text-primary cursor-pointer">
            ← all sessions
          </button>
          <h2 className="text-[14.5px] font-semibold min-w-0 break-words">
            #{task.id.slice(0, 6)} {task.description}
          </h2>
          {blocked ? (
            <span className="flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
              <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              <span className="text-[11.5px] font-medium text-error">blocked</span>
            </span>
          ) : (
            badge && (
              <span
                className={`text-[11px] px-1.5 py-px border tracking-wide flex-none ${badge.className}`}
                title={badge.title ?? task.podMessage ?? undefined}
              >
                {badge.label}
              </span>
            )
          )}
          <span className="text-[11px] text-dim2 border border-line px-1.5 py-px flex-none">{task.status}</span>
          <span className="text-[11.5px] text-dim2 min-w-0 truncate">
            {repoLabel(task)}
            {branch && ` · ${branch}`}
            {heartbeat && (
              <span className={staleTag ? "text-error" : undefined}> · {heartbeat}</span>
            )}
            {task.retryCount > 0 && ` · attempt ${task.retryCount + 1}`}
          </span>
          {prLink && (
            <a
              href={task.prUrl}
              target="_blank"
              rel="noreferrer"
              className={`text-[11.5px] border border-current px-1.5 py-px flex-none ${prLink.className}`}
            >
              {prLink.label}
            </a>
          )}
          <div className="ml-auto flex items-center gap-2 flex-none">
            <span className="text-[10.5px] tracking-[0.1em] text-dim2">DENSITY</span>
            <Segmented value={density} options={DENSITY} onChange={setDensity} />
          </div>
        </div>

        <div className="relative flex-1 min-h-0">
          <div
            ref={feedRef}
            onScroll={feedOnScroll}
            className="absolute inset-0 overflow-y-auto px-4.5 py-4.5 flex flex-col gap-4"
          >
            {/* A proposal has no transcript to decide from — the decision is
                whether to dispatch at all, so it leads the feed. */}
            {task.status === "proposed" && (
              <NotchCard label="◉ PROPOSED — NEEDS APPROVAL" tone="pink" labelBg="bg-base-100">
                <DecisionInline
                  task={task}
                  onOpenSession={() => {}}
                  reload={() => {}}
                  onDismissed={onClosed}
                />
              </NotchCard>
            )}
            <SessionFeed
              entries={entries}
              visibility={visibility}
              density={density}
              busyKey={busyKey}
              onRespond={(seq, decision) =>
                run(
                  () => client.respondToPermission({ taskId, seq, decisionJson: JSON.stringify(decision) }),
                  `permission:${seq}`,
                )
              }
              onAnswer={(seq, answers) =>
                run(() => client.answerQuestion({ taskId, seq, answersJson: JSON.stringify({ answers }) }), `question:${seq}`)
              }
              onPlanFeedback={(text) => sendDiscuss(text)}
            />
            {pendingMessage && (
              <div className="flex gap-2.5 items-baseline opacity-60">
                <span className="text-primary flex-none text-[13.5px]">❯</span>
                <div className="flex-1 min-w-0 text-[13.5px] leading-[1.7] text-text2">
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
              className="absolute bottom-3 left-1/2 -translate-x-1/2 border border-line bg-base-300 px-2 py-1 text-[12px] shadow-md"
              title="Scroll to bottom"
            >
              ↓
              {hasNewAiMessage && (
                <span className="absolute -top-1 -right-1 w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              )}
            </button>
          )}
        </div>

        <div className="flex-none px-4.5 py-3 border-t border-line">
          <ErrorModal message={actionError} onClose={clearActionError} />
          <BypassConfirmModal
            open={bypassOpen}
            onCancel={() => setBypassOpen(false)}
            onConfirm={() => {
              setBypassOpen(false);
              run(() => client.setPermissionMode({ taskId, mode: "bypassPermissions" }), "bypass");
            }}
          />

          {(chipQuestion || slashCommands.length > 0) && (
            <div className="flex gap-2 mb-2.5 flex-wrap">
              {chipQuestion?.options.map((opt) => (
                <button
                  key={opt.label}
                  type="button"
                  disabled={busyKey !== null}
                  title={opt.description}
                  onClick={() =>
                    pendingQuestion &&
                    run(
                      () =>
                        client.answerQuestion({
                          taskId,
                          seq: pendingQuestion.seq,
                          answersJson: JSON.stringify({ answers: { [chipQuestion.question]: opt.label } }),
                        }),
                      `question:${pendingQuestion.seq}`,
                    )
                  }
                  className="border border-pink-line text-error px-3 py-1 text-[11.5px] cursor-pointer hover:bg-pink-chip disabled:opacity-40"
                >
                  {opt.label}
                </button>
              ))}
              {slashCommands.map((c) => (
                <button
                  key={c}
                  type="button"
                  onClick={() => setMessage(`/${c} `)}
                  className="border border-line text-dim px-3 py-1 text-[11.5px] cursor-pointer hover:text-base-content"
                >
                  /{c}
                </button>
              ))}
            </div>
          )}

          <div className="flex gap-3 items-center border border-line px-3 py-2.5 focus-within:border-primary/60">
            <span className="text-primary text-[13.5px]">❯</span>
            <input
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendMessage();
              }}
              disabled={busyKey !== null}
              placeholder="message the agent — / for commands"
              aria-label="message the agent"
              className="flex-1 min-w-0 bg-transparent outline-none text-[13px] placeholder:text-dim2"
            />
            <button
              type="button"
              disabled={busyKey !== null || !message.trim()}
              onClick={sendMessage}
              className="text-[11.5px] text-dim2 hover:text-primary disabled:opacity-40 disabled:hover:text-dim2 cursor-pointer flex-none"
            >
              ⏎ send
            </button>
          </div>

          <div className="flex gap-3.5 mt-2 text-[11px] text-dim2 flex-wrap">
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
            <span className={task.permissionMode === "bypassPermissions" ? "text-warning" : undefined}>
              ▸▸ {task.permissionMode || "default"} permissions
            </span>
          </div>
        </div>
      </div>

      <div className="w-[266px] flex-none overflow-y-auto px-3.5 py-3.5 flex flex-col gap-4.5 min-w-0">
        <TodosPanel todos={todos} blocked={blocked} />
        <ChangesPanel branch={branch} changes={changes} />
        <E2ePanel e2e={e2e} />
        <div className="mt-auto">
          <SessionPanel
            task={task}
            busy={busyKey !== null}
            run={run}
            previewUrl={previewUrl}
            isThotTask={isThot(task)}
            onBypassClick={() => setBypassOpen(true)}
          />
        </div>
      </div>

    </div>
  );
}
