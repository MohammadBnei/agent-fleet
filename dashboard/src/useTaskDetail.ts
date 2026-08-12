import { useEffect, useRef, useState } from "react";
import { isThot } from "./taskKind";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "./connectClient";
import type { Task } from "./gen/agentfleet/v1/core_pb";
import type { GetE2eStatusResponse } from "./gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// Shared data-loading for a single task's session view — used by both the
// desktop TaskDetail and the mobile MobileTaskDetail, which differ only in
// layout, not in what they fetch/subscribe to.
export function useTaskDetail(taskId: string) {
  const [task, setTask] = useState<Task | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  // Full e2e status, not just the preview URL: the card needs the recipe
  // that actually ran plus the pod's live readiness to tell "still
  // installing" apart from "bound the wrong interface and never will".
  const [e2e, setE2e] = useState<GetE2eStatusResponse | null>(null);
  const [branch, setBranch] = useState<string | null>(null);
  // Keyed, not a plain boolean — a click on one PermissionCard/PlanCard used
  // to disable every other pending card and the whole ActionsMenu at once.
  // Each caller passes its own key to run() (a permission/question's own
  // seq, or a fixed string for ActionsMenu's page-level actions, which
  // still disable together on purpose); "actions" callers still tie
  // themselves to busyKey !== null, not a specific key.
  const [busyKey, setBusyKey] = useState<string | null>(null);
  // Optimistic echo for a just-sent human message — client.discuss() plus
  // the streamTranscript round trip to see it reflected back is enough
  // latency to feel laggy, so this shows the message immediately with a
  // spinner instead of waiting. Cleared once the real entry arrives (ref,
  // not state, so the subscribeTranscript closure below always reads the
  // latest value without needing to resubscribe on every keystroke/send).
  const [pendingMessage, setPendingMessage] = useState<string | null>(null);
  const pendingRef = useRef<string | null>(null);
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
    pendingRef.current = null;
    setPendingMessage(null);

    let cancelled = false;
    // Opening a session is what marks it seen (docs/adr/0040) — the
    // difference between `done` (finished while nobody was looking) and
    // plain `idle`. Fire-and-forget: this is a read-receipt, and failing
    // to record one must never block the view from loading.
    client.markSeen({ taskId }).catch(() => {});
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

    // Polled, not fetched once: the interesting transitions all happen after
    // mount — a session gets requested mid-view, and an app takes minutes to
    // go from "installing" to actually serving (a cold bun install measured
    // 782s on the real cluster). A one-shot read showed "not ready" forever.
    // ponytail: fixed 5s poll of one small RPC; swap for a server-push
    // channel only if the e2e card ever needs sub-second freshness.
    function pollE2e() {
      // docs/adr/0037: a thot session never has an e2e pod, so this would
      // poll every 5s forever for something that cannot exist.
      if (isThot(task)) return;
      client
        .getE2eStatus({ taskId })
        .then((res) => {
          if (cancelled) return;
          setE2e(res);
          setPreviewUrl(res.status === "running" && res.previewUrl ? res.previewUrl : null);
        })
        .catch(() => {
          // No active e2e session is the common case — not worth surfacing
          // as a page-level error.
        });
    }
    pollE2e();
    const e2eTimer = setInterval(pollE2e, 5000);

    let unsubscribe = () => {};
    client
      .getTranscript({ taskId, sinceSeq: 0n })
      .then((res) => {
        if (cancelled) return;
        setEntries(res.entries);
        unsubscribe = subscribeTranscript(taskId, res.nextSeq, (entry) => {
          setEntries((prev) => [...prev, entry]);
          if (pendingRef.current !== null && entry.from === "human" && entry.text === pendingRef.current) {
            pendingRef.current = null;
            setPendingMessage(null);
          }
        });
      })
      .catch((err: Error) => !cancelled && setLoadError(err.message));

    return () => {
      cancelled = true;
      clearInterval(e2eTimer);
      unsubscribe();
    };
  }, [taskId]);

  async function run(action: () => Promise<unknown>, key: string) {
    setBusyKey(key);
    setActionError(null);
    try {
      await action();
    } catch (err) {
      setActionError((err as Error).message);
    } finally {
      setBusyKey(null);
    }
  }

  async function sendDiscuss(text: string) {
    pendingRef.current = text;
    setPendingMessage(text);
    setBusyKey("discuss");
    setActionError(null);
    try {
      await client.discuss({ taskId, text });
    } catch (err) {
      setActionError((err as Error).message);
      pendingRef.current = null;
      setPendingMessage(null);
    } finally {
      setBusyKey(null);
    }
  }

  return {
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
    clearActionError: () => setActionError(null),
  };
}
