import { useState, useEffect } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/dashboard_pb";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { enrichTask } from "../mockEnrichment";

type QuestionOption = { label: string; description: string };
type Question = { question: string; header: string; options: QuestionOption[]; multiSelect?: boolean };

// AskUserQuestion (docs/adr/0018) posts `text` as JSON, not prose — these
// parse it defensively so a malformed/future-shaped payload falls back to
// a plain bubble instead of crashing the page.
function parseQuestions(text: string): Question[] | null {
  try {
    const parsed = JSON.parse(text) as { questions?: unknown };
    return Array.isArray(parsed.questions) ? (parsed.questions as Question[]) : null;
  } catch {
    return null;
  }
}

function parseAnswers(text: string): Record<string, string> | null {
  try {
    const parsed = JSON.parse(text) as { answers?: unknown };
    return parsed.answers && typeof parsed.answers === "object" ? (parsed.answers as Record<string, string>) : null;
  } catch {
    return null;
  }
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
        <div className="text-[10px] opacity-50">{entry.from}</div>
        <div className="text-[12px] whitespace-pre-wrap mt-1">{entry.text}</div>
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
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Two states, not one: loadError blocks rendering (nothing to show
  // without a task), actionError is inline while the loaded view stays up.
  // Collapsing these into one `error` state previously left the error
  // banner unreachable — an unconditional `if (!task) return <Loading/>`
  // ran before it, so any load failure hung on "Loading…" forever.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    setTask(null);
    setEntries([]);
    setPreviewUrl(null);
    setLoadError(null);
    setActionError(null);

    let cancelled = false;
    client
      .getTask({ id: taskId })
      .then((res) => {
        if (!cancelled) setTask(res.task ?? null);
      })
      .catch((err: ConnectError) => {
        if (cancelled) return;
        setLoadError(err.code === Code.NotFound ? "Task not found." : err.message);
      });

    client
      .getE2eStatus({ taskId })
      .then((res) => {
        if (!cancelled && res.status === "running" && res.previewUrl) {
          setPreviewUrl(res.previewUrl);
        }
      })
      .catch(() => {
        // No active e2e session is the common case — not worth surfacing as
        // a page-level error.
      });

    let unsubscribe = () => {};
    client
      .getTranscript({ taskId, sinceSeq: 0n })
      .then((res) => {
        if (cancelled) return;
        setEntries(res.entries);
        unsubscribe = subscribeTranscript(taskId, res.nextSeq, (entry) => {
          setEntries((prev) => [...prev, entry]);
        });
      })
      .catch((err: Error) => !cancelled && setLoadError(err.message));

    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, [taskId]);

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    setActionError(null);
    try {
      await action();
    } catch (err) {
      setActionError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (loadError) return <div className="alert alert-error m-4">{loadError}</div>;
  if (!task) return <div className="p-4">Loading…</div>;

  const enrichment = enrichTask(task);
  const siblings = tasks.filter((t) => t.id !== taskId);

  // Only one question is ever pending per task at a time (the planner's
  // tool call blocks on it) — the next ANSWER entry, if any, is always
  // this question's answer. Used both by the inline QuestionCard below and
  // by the quick-reply chip row.
  const pendingQuestionIdx = entries.findIndex(
    (e, i) =>
      e.type === TranscriptEntryType.QUESTION &&
      !entries.slice(i + 1).some((later) => later.type === TranscriptEntryType.ANSWER),
  );
  const pendingQuestion = pendingQuestionIdx >= 0 ? entries[pendingQuestionIdx] : null;
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;

  return (
    <div className="flex flex-col h-full">
      <div className="flex-none px-6 pt-4 pb-3.5 border-b border-base-content/10 flex items-start gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2.5">
            <h2 className="font-display font-semibold text-[19px]">{task.description}</h2>
            <span className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${STATUS_COLOR[task.status] ?? "border-base-content/20"}`}>
              {task.status.toUpperCase()}
            </span>
          </div>
          <div className="flex flex-wrap items-center gap-3.5 mt-2 text-[10px] text-base-content/50">
            <span>#{task.id.slice(0, 6)}</span>
            <span>{task.repo}</span>
            <span>worktree {enrichment.worktree}</span>
            <span>branch {enrichment.branch}</span>
            <span>{enrichment.model}</span>
            <span>{enrichment.tokens}</span>
            <span>started {enrichment.startedAt}</span>
          </div>
        </div>
        <div className="ml-auto flex gap-2 flex-none">
          <button
            type="button"
            className="btn btn-success btn-sm"
            disabled={busy}
            onClick={() => run(() => client.approve({ taskId }))}
          >
            Approve
          </button>
          <button
            type="button"
            className="btn btn-warning btn-sm"
            disabled={busy}
            onClick={() => run(() => client.stop({ taskId }))}
          >
            Stop
          </button>
          <button
            type="button"
            className="btn btn-outline btn-sm"
            disabled={busy}
            onClick={() => run(() => client.killE2e({ taskId }))}
          >
            Kill e2e
          </button>
          {previewUrl && (
            <a href={previewUrl} target="_blank" rel="noreferrer" className="btn btn-outline btn-sm">
              Open code-server
            </a>
          )}
        </div>
      </div>

      {actionError && <div className="alert alert-error mx-6 mt-3">{actionError}</div>}

      <div className="flex flex-1 min-h-0">
        <div className="flex-1 min-w-0 flex flex-col">
          <div className="flex-1 overflow-y-auto px-6 py-5 flex flex-col gap-3">
            {entries.map((entry, idx) => {
              // Rendered inline by its QUESTION entry below, not as its own bubble.
              if (entry.type === TranscriptEntryType.ANSWER) return null;

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

              return (
                <div key={String(entry.seq)} className="max-w-[760px]">
                  <div className="text-[10px] opacity-50">{entry.from}</div>
                  <div className="text-[12px] leading-relaxed whitespace-pre-wrap mt-1 text-base-content/90">
                    {entry.text}
                  </div>
                </div>
              );
            })}
          </div>

          <div className="flex-none px-6 py-3.5 border-t border-base-content/10 bg-base-200 flex flex-col gap-2.5">
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
            <div className="flex items-center gap-2.5 px-3.5 py-3 border border-base-content/15 rounded-lg bg-base-300/40">
              <span className="text-primary font-semibold">&gt;</span>
              <input
                disabled
                placeholder="answer via the question card above — no free-text channel yet"
                className="flex-1 bg-transparent outline-none text-[12px] placeholder:text-base-content/40 cursor-not-allowed"
              />
            </div>
          </div>
        </div>

        <div className="w-[280px] flex-none border-l border-base-content/10 bg-base-200 px-4 py-4 overflow-y-auto flex flex-col gap-5">
          <div>
            <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">TODOS</div>
            <div className="flex flex-col gap-1.5 mt-2.5">
              {enrichment.todos.map((t, i) => (
                <div key={i} className="flex gap-2 items-start text-[11px]">
                  <span className={t.done ? "text-success" : "text-base-content/30"}>{t.done ? "✓" : "○"}</span>
                  <span className={t.done ? "line-through text-base-content/40" : "text-base-content/80"}>
                    {t.text}
                  </span>
                </div>
              ))}
            </div>
          </div>

          <div>
            <div className="flex items-baseline gap-2">
              <span className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">CHANGES</span>
              <span className="text-[9.5px] text-base-content/35">{enrichment.branch}</span>
            </div>
            <div className="flex flex-col gap-2 mt-2.5">
              {enrichment.changes.map((c, i) => (
                <div key={i} className="flex items-center gap-2 text-[10.5px]">
                  <span className="flex-1 text-base-content/70 truncate">{c.path}</span>
                  <span className="text-success">+{c.added}</span>
                  {c.removed > 0 && <span className="text-warning">−{c.removed}</span>}
                </div>
              ))}
            </div>
          </div>

          <div>
            <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold">DECISIONS</div>
            <div className="flex flex-col gap-2 mt-2.5">
              {enrichment.decisions.map((d, i) => (
                <div key={i} className="text-[10.5px] leading-relaxed text-base-content/70">
                  {d.text} <span className="text-base-content/35">· {d.author}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>

      {siblings.length > 0 && (
        <div className="flex-none border-t border-base-content/10 bg-base-200 px-6 py-2.5 flex items-center gap-2.5 overflow-x-auto">
          <span className="text-[9.5px] tracking-[0.1em] text-base-content/40 flex-none">REST OF THE HERD</span>
          {siblings.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => onSelect(t.id)}
              className="flex-none flex items-center gap-2 px-2.5 py-1.5 border border-base-content/10 rounded-md hover:border-base-content/25 cursor-pointer"
            >
              <span className="w-1.5 h-1.5 rounded-full bg-info" />
              <span className="text-[10.5px]">#{t.id.slice(0, 6)}</span>
              <span className="text-[10px] text-base-content/50">{enrichTask(t).currentActivity}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
