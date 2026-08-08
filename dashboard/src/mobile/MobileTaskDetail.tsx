import { useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  parseQuestions,
  parseAnswers,
  findPendingQuestion,
  findPendingPermissions,
  latestSlashCommands,
  parseSdkSystemInfo,
  parseSdkToolResult,
  parseSdkResultSummary,
  buildToolCallPairs,
  pairedResultSeqs,
  latestTodos,
  asDisplayMarkdown,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useLocalStorageState } from "../useLocalStorageState";
import { useAtBottom } from "../useAtBottom";
import { ToolCallItem } from "../components/ToolCallItem";
import { ToolCallLine } from "../components/ToolCallLine";
import { PlanCard } from "../components/PlanCard";
import { PermissionCard } from "../components/PermissionCard";
import { ErrorModal } from "../components/ErrorModal";
import { Markdown } from "../components/Markdown";
import { JsonView } from "../components/JsonView";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Modal } from "../components/Modal";
import { ActionsMenu } from "../components/ActionsMenu";
import { prBadge } from "../pages/TaskList";

// Mirrors the "herd" mock's phone session screen (Agent Fleet Mobile.dc.html)
// minus device chrome. Single-pane (list vs. detail), unlike desktop's
// side-by-side split — onBack returns to MobileTaskList.

function MobileQuestionCard({
  entry,
  answer,
  busy,
  onSubmit,
}: {
  entry: TranscriptEntry;
  answer: Record<string, string> | null;
  busy: boolean;
  onSubmit: (answers: Record<string, string>) => void;
}) {
  const [selected, setSelected] = useState<Record<number, string[]>>({});
  const questions = parseQuestions(entry.text);

  if (!questions) {
    return (
      <div className="px-3 py-2.5 rounded-lg bg-base-200/60 border border-base-content/10">
        <div className="text-[12.5px] whitespace-pre-wrap">{entry.text}</div>
      </div>
    );
  }

  function toggle(qIndex: number, label: string, multiSelect: boolean | undefined) {
    setSelected((prev) => {
      const current = prev[qIndex] ?? [];
      if (multiSelect) {
        return {
          ...prev,
          [qIndex]: current.includes(label) ? current.filter((l) => l !== label) : [...current, label],
        };
      }
      return { ...prev, [qIndex]: [label] };
    });
  }

  const allAnswered = questions.every((_, i) => (selected[i] ?? []).length > 0);

  function submit() {
    if (!questions) return;
    const answers: Record<string, string> = {};
    questions.forEach((q, i) => {
      answers[q.question] = (selected[i] ?? []).join(", ");
    });
    onSubmit(answers);
  }

  return (
    <div
      className="rounded-xl border p-3.5"
      style={{
        borderColor: "rgba(224,169,78,.45)",
        background: "linear-gradient(180deg,rgba(224,169,78,.1),rgba(224,169,78,.03))",
      }}
    >
      <div className="flex items-center gap-2">
        {!answer && <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />}
        <span className="text-[9px] tracking-[0.09em] font-semibold text-primary">
          {answer ? "ANSWERED" : "QUESTION"}
        </span>
      </div>
      <div className="flex flex-col gap-3.5 mt-2.5">
        {questions.map((q, qIndex) => (
          <fieldset key={qIndex} className="flex flex-col gap-2">
            <legend className="text-[13px] leading-relaxed text-base-content">{q.question}</legend>
            {answer ? (
              <div className="badge badge-outline">{answer[q.question] ?? "—"}</div>
            ) : q.multiSelect ? (
              q.options.map((opt) => (
                <label key={opt.label} className="flex items-start gap-2 cursor-pointer text-[12px]">
                  <input
                    type="checkbox"
                    className="checkbox checkbox-sm mt-0.5"
                    checked={(selected[qIndex] ?? []).includes(opt.label)}
                    onChange={() => toggle(qIndex, opt.label, true)}
                  />
                  <span className="font-medium">{opt.label}</span>
                </label>
              ))
            ) : (
              <div className="flex flex-col gap-1.5">
                {q.options.map((opt) => {
                  const active = (selected[qIndex] ?? []).includes(opt.label);
                  return (
                    <button
                      key={opt.label}
                      type="button"
                      onClick={() => toggle(qIndex, opt.label, false)}
                      className={`px-3 py-2 rounded-lg text-[12px] border text-left min-h-9 ${
                        active
                          ? "border-primary/60 bg-primary/15 text-primary"
                          : "border-base-content/15 text-base-content/70"
                      }`}
                    >
                      {opt.label}
                    </button>
                  );
                })}
              </div>
            )}
          </fieldset>
        ))}
        {!answer && (
          <button type="button" className="btn btn-primary btn-sm" disabled={busy || !allAnswered} onClick={submit}>
            Submit answer
          </button>
        )}
      </div>
    </div>
  );
}

// Mirrors desktop TaskDetail's EntryBubble at mobile's smaller text scale —
// same reliability-findings.md #0 rationale ("relay everything, let the UI
// decide"), cosmetic only. ASSISTANT (tool_use) is handled by the feed loop
// itself via ToolCallItem, not here — every ASSISTANT entry always becomes a
// pair. USER stays here only as the orphaned-tool_result fallback (no
// matching call, e.g. a truncated history) — the normal case is skipped by
// the feed loop too, already folded into its pair's ToolCallItem.
function MobileEntryBubble({ entry }: { entry: TranscriptEntry }) {
  if (entry.type === TranscriptEntryType.SYSTEM) {
    const info = parseSdkSystemInfo(entry.text);
    return (
      <div className="text-[10px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-ghost badge-xs">session</span>
        {info?.model ? `started · ${info.model}` : "session started"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.USER) {
    const tr = parseSdkToolResult(entry.text);
    return (
      <div
        className={`rounded-md border px-2.5 py-2 ${tr?.isError ? "border-error/40 bg-error/5" : "border-base-content/10 bg-base-200/40"}`}
      >
        {typeof tr?.content === "string" ? (
          <pre className="text-[10.5px] whitespace-pre-wrap font-mono text-base-content/70">{tr.content}</pre>
        ) : (
          <JsonView value={tr?.content ?? entry.text} />
        )}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.RESULT) {
    const r = parseSdkResultSummary(entry.text);
    return (
      <div className="text-[10px] text-base-content/40">
        {r ? `${r.numTurns ?? "?"} turns · $${(r.totalCostUsd ?? 0).toFixed(3)}` : entry.text}
      </div>
    );
  }
  // Mirrors desktop EntryBubble's new cases — a real backend-verified event
  // gets the same compact log-line treatment as SYSTEM/RESULT, distinct from
  // the generic prose bubble the model's own narration of the same claim
  // would otherwise render identically to.
  if (entry.type === TranscriptEntryType.APPROVE) {
    return (
      <div className="text-[10px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-success badge-xs">approved</span>
        plan approved — implementing
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.ABORT) {
    return (
      <div className="text-[10px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-warning badge-xs">stopped</span>
        {entry.text || "task stopped"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.PERMISSION_MODE) {
    return (
      <div className="text-[10px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-info badge-xs">mode</span>
        permission mode → {entry.text}
      </div>
    );
  }
  return (
    <div>
      <div className={`text-[12.5px] leading-relaxed ${entry.from === "human" ? "text-primary" : "text-base-content/90"}`}>
        <Markdown text={asDisplayMarkdown(entry)} />
      </div>
    </div>
  );
}

export function MobileTaskDetail({
  taskId,
  onBack,
  onDelete,
}: {
  taskId: string;
  onBack: () => void;
  onDelete: () => void;
}) {
  const { task, entries, previewUrl, busy, loadError, actionError, pendingMessage, run, sendDiscuss, clearActionError } =
    useTaskDetail(taskId);
  const [message, setMessage] = useState("");
  const [bypassOpen, setBypassOpen] = useState(false);
  const [actionsOpen, setActionsOpen] = useState(false);
  // Plain show/hide for tool calls in this feed — settable here (via the
  // Actions modal, no sidebar on mobile) or from desktop's TOOL CALLS panel
  // header, same localStorage key either way.
  const [hideToolsInFeed, setHideToolsInFeed] = useLocalStorageState<boolean>("taskDetail.hideToolsInFeed", false);
  const [hideChangesInFeed, setHideChangesInFeed] = useLocalStorageState<boolean>("taskDetail.hideChangesInFeed", false);
  const { ref: feedRef, atBottom: feedAtBottom, onScroll: feedOnScroll, scrollToBottom: feedScrollToBottom } =
    useAtBottom<HTMLDivElement>();

  // See TaskDetail.tsx's identical effect for the rationale: new human
  // entries jump to the bottom; new agent entries follow too when the
  // reader was already at the bottom, otherwise just pulse the button.
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
  useEffect(() => {
    if (feedAtBottom) setHasNewAiMessage(false);
  }, [feedAtBottom]);

  // See TaskDetail.tsx's identical effect: land on the latest message when
  // a task is opened, instant (not smooth) since this is "arriving," not
  // "a new message came in."
  const scrolledForTaskRef = useRef<string | null>(null);
  useEffect(() => {
    if (entries.length === 0 || scrolledForTaskRef.current === taskId) return;
    scrolledForTaskRef.current = taskId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [taskId, entries, feedRef]);

  if (loadError) {
    return (
      <div className="p-4">
        <button type="button" onClick={onBack} className="text-[13px] text-base-content/60 mb-3">
          ‹ back
        </button>
        <div className="alert alert-error">{loadError}</div>
      </div>
    );
  }
  if (!task) return <div className="p-4">Loading…</div>;

  const todos = latestTodos(entries) ?? [];
  const done = todos.filter((t) => t.status === "completed").length;
  const prLink = prBadge(task);
  const pendingQuestion = findPendingQuestion(entries);
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;
  const blockedTodo = pendingQuestion
    ? (todos.find((t) => t.status === "in_progress") ?? todos.find((t) => t.status !== "completed"))
    : null;
  const pendingPermissions = findPendingPermissions(entries);
  // Tool calls stay inline in the mobile feed (unlike desktop's right-panel
  // move) — paired via the same call<->output id correlation, one
  // collapsible ToolCallItem per call instead of two separate bubbles.
  const toolCallPairs = buildToolCallPairs(entries);
  const toolCallPairsBySeq = new Map(toolCallPairs.map((p) => [p.call.seq, p]));
  const consumedResultSeqs = pairedResultSeqs(toolCallPairs);
  // Lean name-only autocomplete, same rationale as desktop TaskDetail.
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
    <div className="flex flex-col h-full bg-base-100">
      <div className="flex-none px-4 pt-3.5 pb-3 border-b border-base-content/10 bg-base-200">
        <div className="flex items-center gap-2.5">
          <button type="button" onClick={onBack} className="text-[17px] text-base-content/60 w-7 h-8 flex items-center">
            ‹
          </button>
          <div className="min-w-0 flex-1">
            <div className="text-[13px] font-semibold text-base-content leading-tight truncate">
              {task.description}
            </div>
            <div className="text-[10px] text-base-content/50 mt-1 flex items-center gap-2">
              <span>
                #{task.id.slice(0, 6)} · {task.repo}
              </span>
              {prLink && (
                <a href={task.prUrl} target="_blank" rel="noreferrer" className={`text-[9px] font-semibold ${prLink.className}`}>
                  {prLink.label}
                </a>
              )}
            </div>
          </div>
          <button
            type="button"
            onClick={onDelete}
            className="w-8 h-8 border border-base-content/10 rounded-lg flex items-center justify-center text-[13px] text-base-content/60 flex-none"
            title="Delete this session"
          >
            ⋯
          </button>
        </div>
        <div className="flex items-center gap-2.5 mt-3">
          {pendingQuestion && (
            <span className="px-1.5 py-0.5 rounded bg-primary/15 border border-primary/45 text-[9px] font-semibold text-primary tracking-wide">
              WAITING ON YOU
            </span>
          )}
          <div className="flex-1 flex gap-1">
            {todos.map((t, i) => (
              <div
                key={i}
                className={`flex-1 h-1 rounded ${
                  t.status === "completed" ? "bg-secondary" : t.status === "in_progress" ? "bg-primary" : "bg-base-content/10"
                }`}
              />
            ))}
          </div>
          <span className="text-[10px] text-base-content/50 flex-none">
            {done}/{todos.length}
          </span>
        </div>
      </div>

      {blockedTodo && (
        <div className="flex-none px-4 py-2.5 border-b border-base-content/5 bg-base-300/30 flex items-start gap-2">
          <span className="text-[10px] text-primary flex-none mt-0.5">→</span>
          <div className="min-w-0">
            <div className="text-[11.5px] text-base-content/90">{blockedTodo.content}</div>
            <div className="text-[9.5px] text-primary mt-0.5">blocked on your answer</div>
          </div>
        </div>
      )}

      <ErrorModal message={actionError} onClose={clearActionError} />

      <div className="relative flex-1 min-h-0">
      <div ref={feedRef} onScroll={feedOnScroll} className="absolute inset-0 overflow-y-auto px-4 py-3.5 flex flex-col gap-3">
        {entries.map((entry, idx) => {
          if (entry.type === TranscriptEntryType.ANSWER) return null;
          if (entry.type === TranscriptEntryType.TOOL_CALL) {
            if (hideChangesInFeed) return null;
            return <ToolCallLine key={String(entry.seq)} entry={entry} />;
          }
          if (entry.type === TranscriptEntryType.PERMISSION_RESPONSE) return null;
          if (entry.type === TranscriptEntryType.PERMISSION_REQUEST) {
            let parsed: { tool?: string; input?: unknown } = {};
            try {
              parsed = JSON.parse(entry.text);
            } catch {
              return null;
            }
            if (typeof parsed.tool !== "string") return null;
            const isPending = pendingPermissions.some((p) => p.entry.seq === entry.seq);
            const respond = (decision: { behavior: "allow" | "deny"; message?: string }) =>
              run(() => client.respondToPermission({ taskId, seq: entry.seq, decisionJson: JSON.stringify(decision) }));
            if (parsed.tool === "ExitPlanMode") {
              const plan = (parsed.input as { plan?: string } | undefined)?.plan;
              if (typeof plan !== "string") return null;
              return (
                <PlanCard
                  key={String(entry.seq)}
                  plan={plan}
                  pending={isPending}
                  busy={busy}
                  onApprove={() => respond({ behavior: "allow" })}
                  onFeedback={(text) => sendDiscuss(text)}
                  edgeClassName="-mx-4 px-4"
                />
              );
            }
            return (
              <PermissionCard
                key={String(entry.seq)}
                tool={parsed.tool}
                input={parsed.input}
                pending={isPending}
                busy={busy}
                onAllow={() => respond({ behavior: "allow" })}
                onDeny={(message) => respond({ behavior: "deny", message })}
                edgeClassName="-mx-4 px-4"
              />
            );
          }
          if (entry.type === TranscriptEntryType.ASSISTANT) {
            if (hideToolsInFeed) return null;
            const pair = toolCallPairsBySeq.get(entry.seq);
            return pair ? <ToolCallItem key={String(entry.seq)} pair={pair} /> : null;
          }
          // Normal case: already rendered inside its pair's ToolCallItem
          // above. Falls through to MobileEntryBubble's orphan handling
          // only if no matching call was found.
          if (entry.type === TranscriptEntryType.USER && consumedResultSeqs.has(entry.seq)) return null;
          if (entry.type === TranscriptEntryType.QUESTION) {
            const answerEntry = entries.slice(idx + 1).find((e) => e.type === TranscriptEntryType.ANSWER);
            return (
              <MobileQuestionCard
                key={String(entry.seq)}
                entry={entry}
                answer={answerEntry ? parseAnswers(answerEntry.text) : null}
                busy={busy}
                onSubmit={(answers) =>
                  run(() => client.answerQuestion({ taskId, seq: entry.seq, answersJson: JSON.stringify({ answers }) }))
                }
              />
            );
          }
          return (
            <div key={String(entry.seq)}>
              <MobileEntryBubble entry={entry} />
            </div>
          );
        })}
        {pendingMessage && (
          <div className="opacity-60">
            <div className="text-[12.5px] leading-relaxed text-primary flex items-start gap-2">
              <div className="flex-1 min-w-0">
                <Markdown text={asDisplayMarkdown({ from: "human", text: pendingMessage })} />
              </div>
              <span className="loading loading-spinner loading-xs flex-none mt-1" />
            </div>
          </div>
        )}
      </div>
      {!feedAtBottom && (
        <button
          type="button"
          onClick={feedScrollToBottom}
          className="btn btn-circle btn-xs absolute bottom-3 left-1/2 -translate-x-1/2 bg-base-300 border-base-content/20 shadow-md"
          title="Scroll to bottom"
        >
          ↓
          {hasNewAiMessage && (
            <span className="absolute -top-0.5 -right-0.5 w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />
          )}
        </button>
      )}
      </div>

      <div className="flex-none border-t border-base-content/10 bg-base-200 px-4 pt-3 pb-2.5 flex flex-col gap-2.5">
        {chipQuestion && (
          <div className="flex gap-1.5 overflow-x-auto">
            {chipQuestion.options.map((opt) => (
              <button
                key={opt.label}
                type="button"
                disabled={busy}
                onClick={() =>
                  pendingQuestion &&
                  run(() =>
                    client.answerQuestion({
                      taskId,
                      seq: pendingQuestion.seq,
                      answersJson: JSON.stringify({ answers: { [chipQuestion.question]: opt.label } }),
                    }),
                  )
                }
                className="flex-none px-3.5 py-2 rounded-2xl border border-primary/45 text-[11px] text-primary font-medium min-h-9 disabled:opacity-40"
              >
                {opt.label}
              </button>
            ))}
          </div>
        )}
        <BypassConfirmModal
          open={bypassOpen}
          onCancel={() => setBypassOpen(false)}
          onConfirm={() => {
            setBypassOpen(false);
            run(() => client.setPermissionMode({ taskId, mode: "bypassPermissions" }));
          }}
        />
        {slashCommands.length > 0 && (
          <div className="flex gap-1.5 overflow-x-auto">
            {slashCommands.map((c) => (
              <button
                key={c}
                type="button"
                onClick={() => setMessage(`/${c} `)}
                className="flex-none px-3.5 py-2 rounded-2xl border border-base-content/15 text-[11px] text-base-content/70 min-h-9"
              >
                /{c}
              </button>
            ))}
          </div>
        )}
        <div className="flex items-stretch gap-2">
          <div className="flex-1 flex items-center gap-2.5 px-3.5 py-3 border border-base-content/15 rounded-lg bg-base-300/40 min-h-11">
            <span className="text-primary font-semibold text-[13px]">&gt;</span>
            <input
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") sendMessage();
              }}
              disabled={busy}
              placeholder="message the agent…"
              className="flex-1 bg-transparent outline-none text-[12px] placeholder:text-base-content/40"
            />
            <button
              type="button"
              disabled={busy || !message.trim()}
              onClick={sendMessage}
              className="text-[11px] text-primary font-medium disabled:opacity-30 disabled:cursor-not-allowed"
            >
              Send
            </button>
          </div>
          <button
            type="button"
            onClick={() => setActionsOpen(true)}
            title="Actions"
            className="flex-none flex items-center justify-center px-3 border border-base-content/15 rounded-lg text-base-content/60 hover:text-base-content cursor-pointer"
          >
            ⚙
          </button>
          <Modal open={actionsOpen} onClose={() => setActionsOpen(false)}>
            <h3 className="font-semibold text-base mb-3">Actions</h3>
            <ActionsMenu
              taskId={taskId}
              busy={busy}
              run={run}
              previewUrl={previewUrl}
              currentMode={task.permissionMode}
              podPhase={task.podPhase}
              onBypassClick={() => {
                setActionsOpen(false);
                setBypassOpen(true);
              }}
              hideToolsInFeed={hideToolsInFeed}
              onHideToolsInFeedChange={setHideToolsInFeed}
              hideChangesInFeed={hideChangesInFeed}
              onHideChangesInFeedChange={setHideChangesInFeed}
            />
          </Modal>
        </div>
      </div>
    </div>
  );
}
