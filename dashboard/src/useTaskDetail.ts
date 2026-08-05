import { useEffect, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "./connectClient";
import type { Task } from "./gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// Shared data-loading for a single task's session view — used by both the
// desktop TaskDetail and the mobile MobileTaskDetail, which differ only in
// layout, not in what they fetch/subscribe to.
export function useTaskDetail(taskId: string) {
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [branch, setBranch] = useState<string | null>(null);
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
    setBranch(null);
    setLoadError(null);
    setActionError(null);

    let cancelled = false;
    client
      .getTask({ id: taskId })
      .then((res) => {
        if (cancelled) return;
        setTask(res.task ?? null);
        if (!res.task) return;
        // WorktreeView has a real branch keyed by (repo, task_id) —
        // available immediately, unlike the TOOL_CALL-derived `changes`
        // (computed by the caller from `entries`) which needs the worker
        // to have pushed at least one telemetry snapshot.
        client
          .listWorktrees({})
          .then((wtRes) => {
            if (cancelled) return;
            const wt = wtRes.worktrees.find((w) => w.repo === res.task!.repo && w.taskId === res.task!.id);
            if (wt) setBranch(wt.branch);
          })
          .catch(() => {
            // No worktree yet (task not claimed) is the common case — not
            // worth surfacing as a page-level error.
          });
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

  return { task, entries, previewUrl, branch, busy, loadError, actionError, run, clearActionError: () => setActionError(null) };
}
