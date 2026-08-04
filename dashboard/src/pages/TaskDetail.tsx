import { useState, useEffect } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/dashboard_pb";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";

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
// was submitted instead). One question per fieldset, radio for single-select
// options, checkboxes for multiSelect — same shape as the native tool.
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
      <div className="chat chat-start">
        <div className="chat-header opacity-60">{entry.from}</div>
        <div className="chat-bubble whitespace-pre-wrap">{entry.text}</div>
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
    <div className="card bg-base-100 border border-base-300">
      <div className="card-body gap-3">
        <div className="text-xs opacity-60">planner asked — {answer ? "answered" : "awaiting your answer"}</div>
        {questions.map((q, qIndex) => (
          <fieldset key={qIndex} className="flex flex-col gap-1">
            <legend className="font-medium">
              <span className="badge badge-ghost mr-2">{q.header}</span>
              {q.question}
            </legend>
            {answer ? (
              <div className="badge badge-outline">{answer[q.question] ?? "—"}</div>
            ) : (
              q.options.map((opt) => (
                <label key={opt.label} className="flex items-start gap-2 cursor-pointer">
                  <input
                    type={q.multiSelect ? "checkbox" : "radio"}
                    name={`q-${String(entry.seq)}-${qIndex}`}
                    className={q.multiSelect ? "checkbox checkbox-sm mt-1" : "radio radio-sm mt-1"}
                    checked={(selected[qIndex] ?? []).includes(opt.label)}
                    onChange={() => toggle(qIndex, opt.label, q.multiSelect)}
                  />
                  <span>
                    <span className="font-medium">{opt.label}</span>
                    {opt.description && <span className="opacity-60"> — {opt.description}</span>}
                  </span>
                </label>
              ))
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

export function TaskDetail({ taskId }: { taskId: string }) {
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

  return (
    <div className="flex flex-col gap-4 p-4">
      <div>
        <h2 className="text-xl font-semibold">
          {task.repo}: {task.description}
        </h2>
        <span className="badge badge-outline mt-1">{task.status}</span>
      </div>

      {actionError && <div className="alert alert-error">{actionError}</div>}

      <div className="flex flex-wrap gap-2">
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
          className="btn btn-error btn-sm"
          disabled={busy}
          onClick={() => run(() => client.stop({ taskId }))}
        >
          Stop
        </button>
        <button
          type="button"
          className="btn btn-warning btn-sm"
          disabled={busy}
          onClick={() => run(() => client.killE2e({ taskId }))}
        >
          Kill e2e
        </button>
        {previewUrl && (
          <a
            href={previewUrl}
            target="_blank"
            rel="noreferrer"
            className="btn btn-outline btn-sm"
          >
            Open code-server
          </a>
        )}
      </div>

      <div className="card bg-base-200">
        <div className="card-body gap-2">
          {entries.map((entry, idx) => {
            // Rendered inline by its QUESTION entry below, not as its own bubble.
            if (entry.type === TranscriptEntryType.ANSWER) return null;

            if (entry.type === TranscriptEntryType.QUESTION) {
              // Only one question is ever pending per task at a time (the
              // planner's tool call blocks on it) — the next ANSWER entry,
              // if any, is always this question's answer.
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
              <div key={String(entry.seq)} className="chat chat-start">
                <div className="chat-header opacity-60">{entry.from}</div>
                <div className="chat-bubble whitespace-pre-wrap">{entry.text}</div>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
