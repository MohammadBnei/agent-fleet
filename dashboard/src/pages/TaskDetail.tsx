import { useState } from "react";
import { client } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/dashboard_pb";
import { podStateBadge, staleBadge, heartbeatLabel, prBadge } from "./TaskList";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  parseQuestions,
  parseAnswers,
  findPendingQuestion,
  latestToolCallSummary,
  parseSdkSystemInfo,
  parseSdkResultSummary,
  buildToolCallPairs,
  latestTodos,
  asDisplayMarkdown,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useAtBottom } from "../useAtBottom";
import { ToolCallItem } from "../components/ToolCallItem";
import { ErrorModal } from "../components/ErrorModal";
import { Markdown } from "../components/Markdown";

// Minimal distinct treatment for the raw SDK message types (reliability-
// findings.md #0: "relay everything, let the UI decide") — before this,
// everything but QUESTION/ANSWER fell through to one generic prose bubble.
// ASSISTANT/USER (tool_use/tool_result) aren't handled here — they move to
// the right panel's TOOL CALLS section as paired ToolCallItems instead of
// two unrelated bubbles in the feed. Cosmetic only: no new state, no new RPCs.
function EntryBubble({ entry }: { entry: TranscriptEntry }) {
  if (entry.type === TranscriptEntryType.SYSTEM) {
    const info = parseSdkSystemInfo(entry.text);
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-ghost badge-xs">session</span>
        {info?.model ? `started · ${info.model}` : "session started"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.RESULT) {
    const r = parseSdkResultSummary(entry.text);
    return (
      <div className="text-[10.5px] text-base-content/40">
        {r ? `${r.numTurns ?? "?"} turns · $${(r.totalCostUsd ?? 0).toFixed(3)}` : entry.text}
      </div>
    );
  }
  return (
    <div className="max-w-[760px]">
      <div className="text-[12px] leading-relaxed text-base-content/90">
        <Markdown text={asDisplayMarkdown(entry)} />
      </div>
    </div>
  );
}

// Renders one AskUserQuestion batch as a form (already answered: shows what
// was submitted instead). One question per fieldset, chip-style buttons for
// single-select options (mirrors the "herd" mock's quick-reply chips —
// same real options the native tool sends, just restyled), checkboxes for
// multiSelect since a chip can't represent "toggle and combine" cleanly.
function QuestionCard({
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
      <div className="px-3 py-2 rounded-md bg-base-200/60 border border-base-content/10">
        <div className="text-[12px] whitespace-pre-wrap">{entry.text}</div>
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
      className="rounded-lg border p-4 max-w-[760px]"
      style={{
        borderColor: "rgba(224,169,78,.45)",
        background: "linear-gradient(180deg,rgba(224,169,78,.09),rgba(224,169,78,.03))",
      }}
    >
      <div className="flex items-center gap-2">
        {!answer && <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />}
        <span className="text-[9.5px] tracking-[0.1em] font-semibold text-primary">
          {answer ? "ANSWERED" : "QUESTION"}
        </span>
      </div>
      <div className="flex flex-col gap-4 mt-3">
        {questions.map((q, qIndex) => (
          <fieldset key={qIndex} className="flex flex-col gap-2">
            <legend className="text-[13px] leading-relaxed text-base-content">
              <span className="badge badge-ghost badge-sm mr-2">{q.header}</span>
              {q.question}
            </legend>
            {answer ? (
              <div className="badge badge-outline">{answer[q.question] ?? "—"}</div>
            ) : q.multiSelect ? (
              q.options.map((opt) => (
                <label key={opt.label} className="flex items-start gap-2 cursor-pointer text-[11px]">
                  <input
                    type="checkbox"
                    className="checkbox checkbox-sm mt-0.5"
                    checked={(selected[qIndex] ?? []).includes(opt.label)}
                    onChange={() => toggle(qIndex, opt.label, true)}
                  />
                  <span>
                    <span className="font-medium">{opt.label}</span>
                    {opt.description && <span className="opacity-60"> — {opt.description}</span>}
                  </span>
                </label>
              ))
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {q.options.map((opt) => {
                  const active = (selected[qIndex] ?? []).includes(opt.label);
                  return (
                    <button
                      key={opt.label}
                      type="button"
                      title={opt.description}
                      onClick={() => toggle(qIndex, opt.label, false)}
                      className={`px-2.5 py-1 rounded-full text-[10.5px] border cursor-pointer ${
                        active
                          ? "border-primary/60 bg-primary/15 text-primary"
                          : "border-base-content/15 text-base-content/70 hover:border-base-content/30"
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
          <button
            type="button"
            className="btn btn-primary btn-sm self-start"
            disabled={busy || !allAnswered}
            onClick={submit}
          >
            Submit answer
          </button>
        )}
      </div>
    </div>
  );
}

const STATUS_COLOR: Record<string, string> = {
  pending: "text-base-content/60 border-base-content/20 bg-base-content/5",
  claimed: "text-info border-info/45 bg-info/10",
  planning: "text-info border-info/45 bg-info/10",
  implementing: "text-info border-info/45 bg-info/10",
  done: "text-success border-success/45 bg-success/10",
  failed: "text-warning border-warning/45 bg-warning/10",
  cancelled: "text-warning border-warning/45 bg-warning/10",
};

export function TaskDetail({
  taskId,
  tasks,
  onSelect,
}: {
  taskId: string;
  tasks: Task[];
  onSelect: (id: string) => void;
}) {
  const {
    task: fetchedTask,
    entries,
    previewUrl,
    branch,
    busy,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    clearActionError,
  } = useTaskDetail(taskId);
  const [message, setMessage] = useState("");
  const { ref: feedRef, atBottom: feedAtBottom, onScroll: feedOnScroll, scrollToBottom: feedScrollToBottom } =
    useAtBottom<HTMLDivElement>();
  const { ref: todosRef, atBottom: todosAtBottom, onScroll: todosOnScroll, scrollToBottom: todosScrollToBottom } =
    useAtBottom<HTMLDivElement>();

  if (loadError) return <div className="alert alert-error m-4">{loadError}</div>;
  if (!fetchedTask) return <div className="p-4">Loading…</div>;

  // `tasks` is App.tsx's already-polled (5s) list — preferring it here
  // keeps pod_phase/status live without a second poll loop just for this
  // page; falls back to the one-shot fetch for the instant before the next
  // poll tick includes this task (e.g. a just-created task).
  const task = tasks.find((t) => t.id === taskId) ?? fetchedTask;

  const siblings = tasks.filter((t) => t.id !== taskId);
  const podBadge = podStateBadge(task);
  const staleTag = staleBadge(task);
  const heartbeat = heartbeatLabel(task);
  const prLink = prBadge(task);

  function sendMessage() {
    const text = message.trim();
    if (!text) return;
    setMessage("");
    sendDiscuss(text);
  }

  // Used both by the inline QuestionCard below and the quick-reply chip row.
  const pendingQuestion = findPendingQuestion(entries);
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;
  const changes = latestToolCallSummary(entries)?.files ?? null;
  const toolCallPairs = buildToolCallPairs(entries);
  const todos = latestTodos(entries);

  return (
    <div className="grid grid-rows-[auto_1fr] h-full">
      <div className="px-6 pt-4 pb-3.5 border-b border-base-content/10">
        <div className="flex items-center gap-2.5">
          <h2 className="font-display font-semibold text-[19px]">{task.description}</h2>
          <span className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${STATUS_COLOR[task.status] ?? "border-base-content/20"}`}>
            {task.status.toUpperCase()}
          </span>
          {podBadge && (
            <span
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${podBadge.className}`}
              title={task.podMessage || undefined}
            >
              POD {podBadge.label}
            </span>
          )}
          {staleTag && (
            <span
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${staleTag.className}`}
              title={staleTag.title}
            >
              {staleTag.label}
            </span>
          )}
          {prLink && (
            <a
              href={task.prUrl}
              target="_blank"
              rel="noreferrer"
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide border-current ${prLink.className}`}
            >
              {prLink.label}
            </a>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-3.5 mt-2 text-[10px] text-base-content/50">
          <span>#{task.id.slice(0, 6)}</span>
          <span>{task.repo}</span>
          {branch && <span>branch {branch}</span>}
          {heartbeat && <span className={staleTag ? "text-error" : undefined}>{heartbeat}</span>}
          {task.retryCount > 0 && <span>attempt {task.retryCount + 1}</span>}
          {task.lastError && (
            <span className="text-warning" title={task.lastError}>
              last error ⓘ
            </span>
          )}
        </div>
      </div>

      <div className="grid grid-cols-[1fr_280px] min-h-0">
        <div className="min-w-0 min-h-0 flex flex-col">
          <div className="relative flex-1 min-h-0">
          <div ref={feedRef} onScroll={feedOnScroll} className="absolute inset-0 overflow-y-auto px-6 py-5 flex flex-col gap-3">
            {entries.map((entry, idx) => {
              // Rendered inline by its QUESTION entry below, not as its own bubble.
              if (entry.type === TranscriptEntryType.ANSWER) return null;
              // Folded into the header's branch and the CHANGES panel
              // above (latestToolCallSummary) — the sidecar's periodic
              // {branch, files[]} push isn't prose meant for the feed.
              if (entry.type === TranscriptEntryType.TOOL_CALL) return null;
              // Moved to the right panel's TOOL CALLS section (paired via
              // buildToolCallPairs) — desktop keeps the main feed to prose/
              // decisions, not raw tool input/output.
              if (entry.type === TranscriptEntryType.ASSISTANT || entry.type === TranscriptEntryType.USER) return null;

              if (entry.type === TranscriptEntryType.QUESTION) {
                const answerEntry = entries.slice(idx + 1).find((e) => e.type === TranscriptEntryType.ANSWER);
                return (
                  <QuestionCard
                    key={String(entry.seq)}
                    entry={entry}
                    answer={answerEntry ? parseAnswers(answerEntry.text) : null}
                    busy={busy}
                    onSubmit={(answers) =>
                      run(() =>
                        client.answerQuestion({ taskId, seq: entry.seq, answersJson: JSON.stringify({ answers }) }),
                      )
                    }
                  />
                );
              }

              return <div key={String(entry.seq)}><EntryBubble entry={entry} /></div>;
            })}
            {pendingMessage && (
              <div className="max-w-[760px] opacity-60">
                <div className="text-[12px] leading-relaxed text-base-content/90 flex items-start gap-2">
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
              className="btn btn-circle btn-xs absolute bottom-3 right-3 bg-base-300 border-base-content/20 shadow-md"
              title="Scroll to bottom"
            >
              ↓
            </button>
          )}
          </div>

          <div className="flex-none px-6 py-3.5 border-t border-base-content/10 bg-base-200 flex flex-col gap-2.5">
            <ErrorModal message={actionError} onClose={clearActionError} />
            {chipQuestion && (
              <div className="flex gap-1.5 items-center flex-wrap">
                <span className="text-[9.5px] tracking-[0.09em] text-base-content/40 mr-1">QUICK</span>
                {chipQuestion.options.map((opt) => (
                  <button
                    key={opt.label}
                    type="button"
                    disabled={busy}
                    title={opt.description}
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
                    className="px-2.5 py-1 rounded-full text-[10.5px] border border-base-content/15 text-base-content/70 hover:border-primary/50 hover:text-primary cursor-pointer disabled:opacity-40"
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
            )}
            <div className="flex items-center gap-2 flex-wrap">
              <button
                type="button"
                className="btn btn-success btn-xs"
                disabled={busy}
                onClick={() => run(() => client.approve({ taskId }))}
              >
                Approve
              </button>
              <button
                type="button"
                className="btn btn-warning btn-xs"
                disabled={busy}
                onClick={() => run(() => client.stop({ taskId }))}
              >
                Stop
              </button>
              <button
                type="button"
                className="btn btn-outline btn-xs"
                disabled={busy}
                onClick={() => run(() => client.killE2e({ taskId }))}
              >
                Kill e2e
              </button>
              {previewUrl && (
                <a href={previewUrl} target="_blank" rel="noreferrer" className="btn btn-outline btn-xs">
                  Open code-server
                </a>
              )}
            </div>
            <div className="flex items-center gap-2.5 px-3.5 py-3 border border-base-content/15 rounded-lg bg-base-300/40">
              <span className="text-primary font-semibold">&gt;</span>
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
          </div>
        </div>

        <div className="border-l border-base-content/10 bg-base-200 px-4 py-4 overflow-y-auto min-h-0 flex flex-col gap-3">
          <div className="rounded-lg border border-base-content/10 bg-base-300/20 p-3">
            <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">TODOS</div>
            <div className="relative">
            <div ref={todosRef} onScroll={todosOnScroll} className="flex flex-col gap-1.5 mt-2.5 max-h-56 overflow-y-auto">
              {todos && todos.length > 0 ? (
                todos.map((t, i) => (
                  <div key={i} className="flex gap-2 items-start text-[11px]">
                    <span
                      className={
                        t.status === "completed"
                          ? "text-success"
                          : t.status === "in_progress"
                            ? "text-primary"
                            : "text-base-content/30"
                      }
                    >
                      {t.status === "completed" ? "✓" : t.status === "in_progress" ? "◐" : "○"}
                    </span>
                    <span
                      className={
                        t.status === "completed"
                          ? "line-through text-base-content/40"
                          : t.status === "in_progress"
                            ? "text-base-content"
                            : "text-base-content/80"
                      }
                    >
                      {t.status === "in_progress" ? t.activeForm : t.content}
                    </span>
                  </div>
                ))
              ) : (
                <div className="text-[10.5px] text-base-content/40">no todos yet</div>
              )}
            </div>
            {!todosAtBottom && (
              <button
                type="button"
                onClick={todosScrollToBottom}
                className="btn btn-circle btn-xs absolute bottom-1 right-1 bg-base-100 border-base-content/20 shadow-md"
                title="Scroll to bottom"
              >
                ↓
              </button>
            )}
            </div>
          </div>

          <div className="rounded-lg border border-base-content/10 bg-base-300/20 p-3">
            <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">TOOL CALLS</div>
            <div className="flex flex-col gap-1.5 mt-2.5">
              {toolCallPairs.length > 0 ? (
                toolCallPairs.map((pair) => <ToolCallItem key={String(pair.call.seq)} pair={pair} />)
              ) : (
                <div className="text-[10.5px] text-base-content/40">no tool calls yet</div>
              )}
            </div>
          </div>

          <div className="rounded-lg border border-base-content/10 bg-base-300/20 p-3">
            <div className="flex items-baseline gap-2">
              <span className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">CHANGES</span>
              {branch && <span className="text-[9.5px] text-base-content/35">{branch}</span>}
            </div>
            <div className="flex flex-col gap-2 mt-2.5">
              {changes && changes.length > 0 ? (
                changes.map((c, i) => (
                  <div key={i} className="flex items-center gap-2 text-[10.5px] min-w-0">
                    <span className="flex-1 text-base-content/70 truncate min-w-0">{c.path}</span>
                    <span className="text-success flex-none">+{c.added}</span>
                    {c.removed > 0 && <span className="text-warning flex-none">−{c.removed}</span>}
                  </div>
                ))
              ) : (
                <div className="text-[10.5px] text-base-content/40">no changes yet</div>
              )}
            </div>
          </div>
        </div>
      </div>

      {siblings.length > 0 && (
        <div className="flex-none border-t border-base-content/10 bg-base-200 px-6 py-2.5 flex items-center gap-2.5 overflow-x-auto">
          <span className="text-[9.5px] tracking-[0.1em] text-base-content/40 flex-none">REST OF THE HERD</span>
          {siblings.map((t) => {
            const siblingPodBadge = podStateBadge(t);
            return (
              <button
                key={t.id}
                type="button"
                onClick={() => onSelect(t.id)}
                className="flex-none flex items-center gap-2 px-2.5 py-1.5 border border-base-content/10 rounded-md hover:border-base-content/25 cursor-pointer"
              >
                <span className="text-[10.5px]">#{t.id.slice(0, 6)}</span>
                <span className={`px-1.5 py-0.5 rounded text-[9px] font-semibold border tracking-wide ${STATUS_COLOR[t.status] ?? "border-base-content/20"}`}>
                  {t.status.toUpperCase()}
                </span>
                {siblingPodBadge && (
                  <span
                    className={`px-1.5 py-0.5 rounded text-[9px] font-semibold border tracking-wide ${siblingPodBadge.className}`}
                    title={t.podMessage || undefined}
                  >
                    {siblingPodBadge.label}
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
