import { useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { isThot, repoLabel } from "../taskKind";
import {
  feedVisibility,
  findPendingPermissions,
  findPendingQuestion,
  latestSlashCommands,
  latestToolCallSummary,
  latestTodos,
  parseQuestions,
  type Density,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { sessionBadge } from "../pages/TaskList";
import { ErrorModal } from "../components/ErrorModal";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Modal } from "../components/Modal";
import { Segmented } from "../components/Segmented";
import { SessionFeed } from "../components/SessionFeed";
import { DiffLines } from "../components/DiffLines";
import { TickBar, todoProgress } from "../components/TickBar";
import { TodosPanel, ChangesPanel, E2ePanel, SessionPanel } from "../components/SessionPanels";
import { summarizeToolInput } from "../transcript";

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
        <button type="button" onClick={onBack} className="text-[13px] text-dim mb-3">
          ←
        </button>
        <div className="border border-pink-line bg-pink-bg px-3 py-2.5 text-[12.5px] text-error">{loadError}</div>
      </div>
    );
  }
  if (!task) return <div className="flex-1 p-4 text-[13px] text-dim">Loading…</div>;

  const blocked = task.liveState === "blocked";
  const badge = sessionBadge(task);
  const visibility = feedVisibility(density, true);
  const todos = latestTodos(entries) ?? [];
  const changes = latestToolCallSummary(entries)?.files ?? null;

  const pendingPermission = findPendingPermissions(entries)[0] ?? null;
  const pendingQuestion = findPendingQuestion(entries);
  const pendingQuestions = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const dockQuestion =
    pendingQuestions && pendingQuestions.length === 1 && !pendingQuestions[0].multiSelect ? pendingQuestions[0] : null;
  const docked = Boolean(pendingPermission || dockQuestion);

  const slashCommands = message.startsWith("/")
    ? (latestSlashCommands(entries) ?? []).filter((c) => c.toLowerCase().startsWith(message.slice(1).toLowerCase()))
    : [];

  function sendMessage() {
    const text = message.trim();
    if (!text) return;
    setMessage("");
    sendDiscuss(text);
  }

  function respond(seq: bigint, behavior: "allow" | "deny", msg?: string) {
    run(
      () => client.respondToPermission({ taskId, seq, decisionJson: JSON.stringify({ behavior, message: msg }) }),
      `permission:${seq}`,
    );
  }

  // Edit/Write get a real diff; anything else its one-line summary. A prompt
  // whose content is unreadable trains people to approve without looking.
  const permInput = (pendingPermission?.input ?? {}) as {
    file_path?: string;
    old_string?: string;
    new_string?: string;
    content?: string;
  };
  const permIsDiff =
    pendingPermission &&
    (pendingPermission.tool === "Edit" || pendingPermission.tool === "Write") &&
    typeof (permInput.new_string ?? permInput.content) === "string";

  return (
    <div className="flex-1 min-h-0 flex flex-col">
      <div className="flex-none px-3.5 py-2.5 border-b border-line">
        <div className="flex items-center gap-2.5">
          <button type="button" onClick={onBack} aria-label="Back to sessions" className="text-[13px] text-dim">
            ←
          </button>
          <span className="text-[13px] font-semibold min-w-0 truncate">
            #{task.id.slice(0, 6)} {task.description}
          </span>
          {blocked ? (
            <span className="ml-auto flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
              <span className="w-[5px] h-[5px] rounded-full bg-error animate-fpulse" />
              <span className="text-[11px] font-medium text-error">blocked</span>
            </span>
          ) : (
            badge && (
              <span className={`ml-auto flex-none text-[10px] px-1 border tracking-wide ${badge.className}`}>
                {badge.label}
              </span>
            )
          )}
        </div>

        <div className="flex items-center gap-2 mt-2">
          <span className="text-[10.5px] text-dim2 min-w-0 truncate">
            {repoLabel(task)}
            {branch && ` · ${branch}`}
          </span>
          <Segmented value={density} options={DENSITY} onChange={setDensity} size="sm" className="ml-auto flex-none" />
        </div>

        <div className="flex items-center gap-2 mt-2.5">
          {todos.length > 0 ? (
            <>
              <TickBar todos={todos} blocked={blocked} className="flex-1" />
              <span className="text-[10.5px] text-dim2 flex-none">{todoProgress(todos)} todos</span>
            </>
          ) : (
            <span className="text-[10.5px] text-dim2 flex-1">no todos yet</span>
          )}
          <button
            type="button"
            onClick={() => setPanelsOpen(true)}
            className="text-[10.5px] text-dim flex-none"
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
          onRespond={(seq, decision) => respond(seq, decision.behavior, decision.message)}
          onAnswer={(seq, answers) =>
            run(() => client.answerQuestion({ taskId, seq, answersJson: JSON.stringify({ answers }) }), `question:${seq}`)
          }
          onPlanFeedback={(text) => sendDiscuss(text)}
        />
        {pendingMessage && (
          <div className="flex gap-2.5 items-baseline opacity-60">
            <span className="text-primary flex-none text-[12.5px]">❯</span>
            <div className="text-[12.5px] leading-[1.7] text-text2 min-w-0 flex-1">{pendingMessage}</div>
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

      {/* The decision dock. Pinned, not inline: a blocked session is stalled
          until someone taps, so its request must not be scrollable-away. */}
      <div
        className={`flex-none ${
          docked ? "mt-2 border-t border-pink-line bg-pink-bg relative" : "border-t border-line"
        }`}
      >
        {docked && pendingPermission && (
          <>
            <div className="absolute -top-[7px] left-3.5 px-[7px] bg-base-200 text-error text-[10px] tracking-[0.1em] whitespace-nowrap">
              ◉ PERMISSION · {pendingPermission.tool.toUpperCase()}
            </div>
            <div className="px-3.5 pt-3.5">
              <div className="text-[11px] text-dim break-all">
                {pendingPermission.tool}
                {permInput.file_path && (
                  <>
                    {" · "}
                    <span className="text-text2">{permInput.file_path}</span>
                  </>
                )}
              </div>
              <div className="mt-2">
                {permIsDiff ? (
                  <DiffLines
                    before={permInput.old_string ?? ""}
                    after={(permInput.new_string ?? permInput.content) as string}
                    maxLines={3}
                    compact
                  />
                ) : (
                  <div className="border border-line bg-code px-2.5 py-1 text-[11.5px] text-text2 whitespace-pre-wrap break-all">
                    {summarizeToolInput(pendingPermission.input)}
                  </div>
                )}
              </div>
              <div className="flex gap-2.5 mt-3">
                <button
                  type="button"
                  disabled={busyKey !== null}
                  onClick={() => respond(pendingPermission.entry.seq, "allow")}
                  className="flex-1 py-3 text-center text-[13.5px] font-semibold bg-primary text-primary-content disabled:opacity-50"
                >
                  allow
                </button>
                <button
                  type="button"
                  disabled={busyKey !== null}
                  onClick={() => respond(pendingPermission.entry.seq, "deny", "denied")}
                  className="flex-1 py-3 text-center text-[13.5px] border border-acc-line disabled:opacity-50"
                >
                  deny
                </button>
              </div>
            </div>
          </>
        )}

        {docked && !pendingPermission && dockQuestion && pendingQuestion && (
          <>
            <div className="absolute -top-[7px] left-3.5 px-[7px] bg-base-200 text-error text-[10px] tracking-[0.1em] whitespace-nowrap">
              ◉ QUESTION
            </div>
            <div className="px-3.5 pt-3.5">
              <div className="text-[13px] leading-[1.6]">{dockQuestion.question}</div>
              <div className="flex flex-col gap-2 mt-3">
                {dockQuestion.options.map((opt) => (
                  <button
                    key={opt.label}
                    type="button"
                    disabled={busyKey !== null}
                    onClick={() =>
                      run(
                        () =>
                          client.answerQuestion({
                            taskId,
                            seq: pendingQuestion.seq,
                            answersJson: JSON.stringify({ answers: { [dockQuestion.question]: opt.label } }),
                          }),
                        `question:${pendingQuestion.seq}`,
                      )
                    }
                    className="w-full text-left border border-acc-line px-3.5 py-3 text-[13px] disabled:opacity-50"
                  >
                    {opt.label}
                    {opt.description && <div className="text-[11px] text-dim2 mt-0.5">{opt.description}</div>}
                  </button>
                ))}
              </div>
            </div>
          </>
        )}

        {(docked || slashCommands.length > 0) && (
          <div className="flex gap-2 px-3.5 pt-2.5 overflow-x-auto">
            {docked && pendingPermission && (
              <button
                type="button"
                disabled={busyKey !== null}
                onClick={() => respond(pendingPermission.entry.seq, "deny", "use the fixture")}
                className="flex-none border border-line text-dim px-2.5 py-1.5 text-[11px] whitespace-nowrap"
              >
                deny — use the fixture
              </button>
            )}
            <button
              type="button"
              disabled={busyKey !== null}
              onClick={() => run(() => client.interrupt({ taskId }), "actions")}
              className="flex-none border border-line text-dim px-2.5 py-1.5 text-[11px] whitespace-nowrap"
            >
              /interrupt
            </button>
            <button
              type="button"
              disabled={busyKey !== null}
              onClick={() => run(() => client.setPermissionMode({ taskId, mode: "plan" }), "actions")}
              className="flex-none border border-line text-dim px-2.5 py-1.5 text-[11px] whitespace-nowrap"
            >
              /mode plan
            </button>
            {slashCommands.map((c) => (
              <button
                key={c}
                type="button"
                onClick={() => setMessage(`/${c} `)}
                className="flex-none border border-line text-dim px-2.5 py-1.5 text-[11px] whitespace-nowrap"
              >
                /{c}
              </button>
            ))}
          </div>
        )}

        <div className="px-3.5 py-2.5">
          <div className="flex gap-2.5 items-center border border-line bg-base-200 px-3 py-2.5 focus-within:border-primary/60">
            <span className="text-primary text-[13px]">❯</span>
            <input
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendMessage();
              }}
              disabled={busyKey !== null}
              placeholder="message the agent"
              aria-label="message the agent"
              className="flex-1 min-w-0 bg-transparent outline-none text-[12.5px] placeholder:text-dim2"
            />
            <button
              type="button"
              disabled={busyKey !== null || !message.trim()}
              onClick={sendMessage}
              className="text-[11px] text-dim2 disabled:opacity-40 flex-none"
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
            className="border border-pink-line text-error px-3 py-2.5 text-[12px] self-start"
          >
            Delete session
          </button>
        </div>
      </Modal>
    </div>
  );
}
