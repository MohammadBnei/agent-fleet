import { useEffect, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";

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
          {entries.map((entry) => (
            <div key={String(entry.seq)} className="chat chat-start">
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
