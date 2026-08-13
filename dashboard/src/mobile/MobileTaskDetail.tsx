import { useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { isThot, repoLabel } from "../taskKind";
import {
  feedVisibility,
  latestSlashCommands,
  latestToolCallSummary,
  latestTodos,
  type Density,
  hasPendingDecision,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { sessionBadge } from "../pages/TaskList";
import { ActionButton } from "../components/ActionButton";
import { DecisionDock } from "../components/DecisionDock";
import { ErrorModal } from "../components/ErrorModal";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Modal } from "../components/Modal";
import { Segmented } from "../components/Segmented";
import { SessionFeed } from "../components/SessionFeed";
import { TickBar, todoProgress } from "../components/TickBar";
import { TodosPanel, ChangesPanel, E2ePanel, SessionPanel } from "../components/SessionPanels";

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

export function MobileTaskDetail({
  taskId,
  onBack,
  onDelete,
}: {
  taskId: string;
  onBack: () => void;
  onDelete: () => void;
}) {
  const {
    task,
    entries,
    previewUrl,
    e2e,
    branch,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    respondToPermission,
    answerQuestion,
    clearActionError,
  } = useTaskDetail(taskId);
  const [message, setMessage] = useState("");
  const [panelsOpen, setPanelsOpen] = useState(false);
  const [bypassOpen, setBypassOpen] = useState(false);
  const [density, setDensity] = useLocalStorageState<Density>("taskDetail.density", "everything");
  const { ref: feedRef, atBottom, onScroll, scrollToBottom } = useAtBottom<HTMLDivElement>();

  const prevLen = useRef<number | null>(null);
  useEffect(() => {
    if (prevLen.current === null) {
      prevLen.current = entries.length;
      return;
    }
    if (entries.length > prevLen.current) {
      const latest = entries[entries.length - 1];
      if (latest.from === "human" || atBottom) scrollToBottom();
    }
    prevLen.current = entries.length;
  }, [entries, atBottom, scrollToBottom]);

  const scrolledFor = useRef<string | null>(null);
  useEffect(() => {
    if (entries.length === 0 || scrolledFor.current === taskId) return;
    scrolledFor.current = taskId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [taskId, entries, feedRef]);

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
  if (!task) return <div className="flex-1 p-4 text-base text-dim">Loading…</div>;

  const blocked = task.liveState === "blocked";
  const badge = sessionBadge(task);
  const visibility = feedVisibility(density, true);
  const todos = latestTodos(entries) ?? [];
  const changes = latestToolCallSummary(entries)?.files ?? null;

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
      <div className="flex-none px-3.5 py-2.5 border-b border-line">
        <div className="flex items-center gap-2.5">
          <button type="button" onClick={onBack} aria-label="Back to sessions" className="text-base text-dim">
            ←
          </button>
          <span className="text-base font-semibold min-w-0 truncate">
            #{task.id.slice(0, 6)} {task.description}
          </span>
          {blocked ? (
            <span className="ml-auto flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
              <span className="w-[5px] h-[5px] rounded-full bg-error animate-fpulse" />
              <span className="text-xs font-medium text-error">blocked</span>
            </span>
          ) : (
            badge && (
              <span className={`ml-auto flex-none text-2xs px-1 border tracking-wide ${badge.className}`}>
                {badge.label}
              </span>
            )
          )}
        </div>

        <div className="flex items-center gap-2 mt-2">
          <span className="text-2xs text-dim2 min-w-0 truncate">
            {repoLabel(task)}
            {branch && ` · ${branch}`}
          </span>
          <Segmented value={density} options={DENSITY} onChange={setDensity} size="sm" className="ml-auto flex-none" />
        </div>

        <div className="flex items-center gap-2 mt-2.5">
          {todos.length > 0 ? (
            <>
              <TickBar todos={todos} blocked={blocked} className="flex-1" />
              <span className="text-2xs text-dim2 flex-none">{todoProgress(todos)} todos</span>
            </>
          ) : (
            <span className="text-2xs text-dim2 flex-1">no todos yet</span>
          )}
          <button
            type="button"
            onClick={() => setPanelsOpen(true)}
            className="text-2xs text-dim flex-none"
          >
            panels ▸
          </button>
        </div>
      </div>

      <div ref={feedRef} onScroll={onScroll} className="flex-1 min-h-0 overflow-y-auto px-3.5 py-3.5 flex flex-col gap-3.5">
        <SessionFeed
          entries={entries}
          visibility={visibility}
          density={density}
          busyKey={busyKey}
          compact
          dockPendingDecision
          onRespond={respondToPermission}
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
      <BypassConfirmModal
        open={bypassOpen}
        onCancel={() => setBypassOpen(false)}
        onConfirm={() => {
          setBypassOpen(false);
          run(() => client.setPermissionMode({ taskId, mode: "bypassPermissions" }), "bypass");
        }}
      />

      <DecisionDock
        entries={entries}
        busyKey={busyKey}
        compact
        onRespond={respondToPermission}
        onAnswer={answerQuestion}
        onPlanFeedback={(text) => sendDiscuss(text)}
      />

      <div className="flex-none border-t border-line">
        {(docked || slashCommands.length > 0) && (
          <div className="flex gap-2 px-3.5 pt-2.5 overflow-x-auto">
            <ActionButton
              busy={busyKey === "action:interrupt"}
              disabled={busyKey !== null}
              onClick={() => run(() => client.interrupt({ taskId }), "action:interrupt")}
              className="flex-none border border-line text-dim px-2.5 py-1.5 text-xs whitespace-nowrap"
            >
              /interrupt
            </ActionButton>
            <ActionButton
              busy={busyKey === "action:mode"}
              disabled={busyKey !== null}
              onClick={() => run(() => client.setPermissionMode({ taskId, mode: "plan" }), "action:mode")}
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
          <div className="flex gap-2.5 items-center border border-line bg-base-200 px-3 py-2.5 focus-within:border-primary/60">
            <span className="text-primary text-base">❯</span>
            <input
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendMessage();
              }}
              disabled={busyKey !== null}
              placeholder="message the agent"
              aria-label="message the agent"
              className="flex-1 min-w-0 bg-transparent outline-none text-sm placeholder:text-dim2"
            />
            <button
              type="button"
              disabled={busyKey !== null || !message.trim()}
              onClick={sendMessage}
              className="text-xs text-dim2 disabled:opacity-40 flex-none"
            >
              send
            </button>
          </div>
        </div>
      </div>

      {/* Everything the desktop right column carries, as a bottom sheet. */}
      <Modal open={panelsOpen} onClose={() => setPanelsOpen(false)}>
        <div className="flex flex-col gap-5">
          <TodosPanel todos={todos} blocked={blocked} />
          <ChangesPanel branch={branch} changes={changes} />
          <E2ePanel e2e={e2e} />
          <SessionPanel
            task={task}
            busy={busyKey !== null}
            busyKey={busyKey}
            run={run}
            previewUrl={previewUrl}
            isThotTask={isThot(task)}
            onBypassClick={() => {
              setPanelsOpen(false);
              setBypassOpen(true);
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
