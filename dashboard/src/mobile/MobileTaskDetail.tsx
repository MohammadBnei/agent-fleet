import { useState } from "react";
import { client } from "../connectClient";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { enrichTask } from "../mockEnrichment";
import { parseQuestions, parseAnswers, findPendingQuestion, parseToolCallSummary } from "../transcript";
import { useTaskDetail } from "../useTaskDetail";

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
        <div className="text-[10px] opacity-50">{entry.from}</div>
        <div className="text-[12.5px] whitespace-pre-wrap mt-1">{entry.text}</div>
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

function ToolCallLine({ entry }: { entry: TranscriptEntry }) {
  const summary = parseToolCallSummary(entry.text);
  const files = summary?.files ?? [];
  if (files.length === 0) return null;
  const added = files.reduce((n, f) => n + f.added, 0);
  const removed = files.reduce((n, f) => n + f.removed, 0);
  return (
    <div className="flex items-center gap-2 text-[11px] min-w-0">
      <span className="text-secondary flex-none">⏺</span>
      <span className="text-base-content/50 flex-none">files</span>
      <span className="text-base-content/70 flex-1 truncate min-w-0">
        {files.length} changed{summary?.branch ? ` · ${summary.branch}` : ""}
      </span>
      <span className="text-success flex-none">+{added}</span>
      {removed > 0 && <span className="text-warning flex-none">−{removed}</span>}
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
  const { task, entries, busy, loadError, actionError, run } = useTaskDetail(taskId);

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

  const enrichment = enrichTask(task);
  const done = enrichment.todos.filter((t) => t.done).length;
  const pendingQuestion = findPendingQuestion(entries);
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;
  const blockedTodo = pendingQuestion ? enrichment.todos.find((t) => !t.done) : null;

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
            <div className="text-[10px] text-base-content/50 mt-1">
              #{task.id.slice(0, 6)} · {task.repo}
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
            {enrichment.todos.map((t, i) => (
              <div
                key={i}
                className={`flex-1 h-1 rounded ${
                  t.done ? "bg-secondary" : pendingQuestion && t === blockedTodo ? "bg-primary" : "bg-base-content/10"
                }`}
              />
            ))}
          </div>
          <span className="text-[10px] text-base-content/50 flex-none">
            {done}/{enrichment.todos.length}
          </span>
        </div>
      </div>

      {blockedTodo && (
        <div className="flex-none px-4 py-2.5 border-b border-base-content/5 bg-base-300/30 flex items-start gap-2">
          <span className="text-[10px] text-primary flex-none mt-0.5">→</span>
          <div className="min-w-0">
            <div className="text-[11.5px] text-base-content/90">{blockedTodo.text}</div>
            <div className="text-[9.5px] text-primary mt-0.5">blocked on your answer</div>
          </div>
        </div>
      )}

      {actionError && <div className="alert alert-error mx-4 mt-2 text-[12px]">{actionError}</div>}

      <div className="flex-1 min-h-0 overflow-y-auto px-4 py-3.5 flex flex-col gap-3">
        {entries.map((entry, idx) => {
          if (entry.type === TranscriptEntryType.ANSWER) return null;
          if (entry.type === TranscriptEntryType.TOOL_CALL) {
            return <ToolCallLine key={String(entry.seq)} entry={entry} />;
          }
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
              <div className="text-[10px] opacity-50">{entry.from}</div>
              <div className="text-[12.5px] leading-relaxed whitespace-pre-wrap mt-1 text-base-content/90">
                {entry.text}
              </div>
            </div>
          );
        })}
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
        <div className="flex items-center gap-2.5">
          <div className="flex-1 flex items-center gap-2.5 px-3.5 py-3 border border-base-content/15 rounded-lg bg-base-300/40 min-h-11">
            <span className="text-primary font-semibold text-[13px]">&gt;</span>
            <input
              disabled
              placeholder="answer via the question card above"
              className="flex-1 bg-transparent outline-none text-[12px] placeholder:text-base-content/40 cursor-not-allowed"
            />
          </div>
        </div>
      </div>
    </div>
  );
}
