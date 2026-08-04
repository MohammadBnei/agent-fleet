import { useEffect, useState } from "react";
import {
  approveTask,
  getE2eStatus,
  getTask,
  getTranscript,
  killE2e,
  stopTask,
  subscribeToTranscript,
  type Task,
  type TranscriptEntry,
} from "../api";

export function TaskDetail({ taskId }: { taskId: string }) {
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setTask(null);
    setEntries([]);
    setPreviewUrl(null);
    setError(null);

    let cancelled = false;
    getTask(taskId)
      .then((t) => !cancelled && setTask(t))
      .catch((err: Error) => !cancelled && setError(err.message));

    getE2eStatus(taskId)
      .then((s) => {
        if (!cancelled && s.status === "running" && s.previewUrl) {
          setPreviewUrl(s.previewUrl);
        }
      })
      .catch(() => {
        // No active e2e session is the common case — not worth surfacing as
        // a page-level error.
      });

    let unsubscribe = () => {};
    getTranscript(taskId, 0)
      .then(({ entries: initial, nextSeq }) => {
        if (cancelled) return;
        setEntries(initial);
        unsubscribe = subscribeToTranscript(taskId, nextSeq, (entry) => {
          setEntries((prev) => [...prev, entry]);
        });
      })
      .catch((err: Error) => !cancelled && setError(err.message));

    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, [taskId]);

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await action();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!task) return <div className="p-4">Loading…</div>;

  return (
    <div className="flex flex-col gap-4 p-4">
      <div>
        <h2 className="text-xl font-semibold">
          {task.repo}: {task.description}
        </h2>
        <span className="badge badge-outline mt-1">{task.status}</span>
      </div>

      {error && <div className="alert alert-error">{error}</div>}

      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          className="btn btn-success btn-sm"
          disabled={busy}
          onClick={() => run(() => approveTask(taskId))}
        >
          Approve
        </button>
        <button
          type="button"
          className="btn btn-error btn-sm"
          disabled={busy}
          onClick={() => run(() => stopTask(taskId))}
        >
          Stop
        </button>
        <button
          type="button"
          className="btn btn-warning btn-sm"
          disabled={busy}
          onClick={() => run(() => killE2e(taskId))}
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
          {entries.map((entry) => (
            <div key={entry.seq} className="chat chat-start">
              <div className="chat-header opacity-60">{entry.from}</div>
              <div className="chat-bubble whitespace-pre-wrap">
                {entry.text}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
