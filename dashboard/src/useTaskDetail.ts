import { useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "./connectClient";
import { withOptimistic } from "./transcript";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// Shared data-loading for a single task's session view — used by both the
// desktop TaskDetail and the mobile MobileTaskDetail, which differ only in
// layout, not in what they fetch/subscribe to.
export function useTaskDetail(sessionId: string) {
  const [task, setTask] = useState<Session | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  // Set by the effect below, which owns the actual fetch; a ref rather than
  // state so reassigning it never re-renders.
  const [branch, setBranch] = useState<string | null>(null);
  // The worktree's real path on the shared PVC — shown in the composer's meta
  // line so "where is this actually working" is answerable without a shell.
  // Same (repo, task_id) lookup as `branch`; there is no path on Task itself.
  const [worktreePath, setWorktreePath] = useState<string | null>(null);
  // Keyed, not a plain boolean — a click on one PermissionCard/PlanCard used
  // to disable every other pending card and the whole ActionsMenu at once.
  // Each caller passes its own key to run() (a permission/question's own
  // seq, or a fixed string for ActionsMenu's page-level actions, which
  // still disable together on purpose); "actions" callers still tie
  // themselves to busyKey !== null, not a specific key.
  const [busyKey, setBusyKey] = useState<string | null>(null);
  // Optimistic echo for a just-sent human message — client.postMessage() plus
  // the streamTranscript round trip to see it reflected back is enough
  // latency to feel laggy, so this shows the message immediately with a
  // spinner instead of waiting. Cleared once the real entry arrives (ref,
  // not state, so the subscribeTranscript closure below always reads the
  // latest value without needing to resubscribe on every keystroke/send).
  const [pendingMessage, setPendingMessage] = useState<string | null>(null);
  const pendingRef = useRef<string | null>(null);
  // The same idea as pendingMessage, for decisions: answering a permission
  // is two round trips (the RPC, then the response entry arriving on the
  // stream) before the card stops offering allow/deny. Holding the answer
  // here and merging it into `entries` means every surface that derives
  // from entries — feed card, dock, list row — agrees instantly, and the
  // real entry supersedes it the moment it lands. See withOptimistic.
  const [optimistic, setOptimistic] = useState<TranscriptEntry[]>([]);
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
    setBranch(null);
    setWorktreePath(null);
    setLoadError(null);
    setActionError(null);
    pendingRef.current = null;
    setPendingMessage(null);

    let cancelled = false;
    // Opening a session is what marks it seen (docs/adr/0040) — the
    // difference between `done` (finished while nobody was looking) and
    // plain `idle`. Fire-and-forget: this is a read-receipt, and failing
    // to record one must never block the view from loading.
    client.markSeen({ sessionId }).catch(() => {});
    client
      .getSession({ id: sessionId })
      .then((res) => {
        if (cancelled) return;
        setTask(res.session ?? null);
        if (!res.session) return;
        // The branch lookup is gone with ListWorktrees (docs/adr/0048 §5).
        //
        // It read the branch off a WorktreeView keyed by (repo, task_id),
        // which worked because the FLEET named the branch: `agent/<taskId>`,
        // a convention it owned. It no longer creates branches at all — the
        // agent runs `git checkout -b` for whatever it wants — so there is
        // nothing to look up by task id, and guessing would be worse than
        // showing nothing. `changes`, derived from the worker's own telemetry
        // snapshots, is the honest source for what a session is touching.
      })
      .catch((err: ConnectError) => {
        if (cancelled) return;
        setLoadError(err.code === Code.NotFound ? "Task not found." : err.message);
      });

    // The e2e status poll is gone with the sandbox it described
    // (docs/adr/0048 §6). It polled every 5s for a second pod's phase,
    // preview URL and resolved recipe — none of which exist now that the app
    // runs in the session's own pod, started by the agent with Bash.

    let unsubscribe = () => {};
    client
      .getTranscript({ sessionId, sinceSeq: 0n })
      .then((res) => {
        if (cancelled) return;
        setEntries(res.entries);
        unsubscribe = subscribeTranscript(sessionId, res.nextSeq, (entry) => {
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
      unsubscribe();
    };
  }, [sessionId]);

  // Returns whether the call actually succeeded, so a caller holding
  // optimistic state knows whether to keep or roll it back. Callers that
  // don't care can keep ignoring it.
  async function run(action: () => Promise<unknown>, key: string): Promise<boolean> {
    setBusyKey(key);
    setActionError(null);
    try {
      await action();
      return true;
    } catch (err) {
      setActionError((err as Error).message);
      return false;
    } finally {
      setBusyKey(null);
    }
  }

  // Both decisions go through here so the optimistic entry and the RPC can
  // never drift apart — a card showing "allowed" for a call that failed to
  // send is worse than a slow card.
  function decide(
    seq: bigint,
    type: TranscriptEntryType.PERMISSION_RESPONSE | TranscriptEntryType.ANSWER,
    text: string,
    action: () => Promise<unknown>,
    key: string,
  ) {
    const echo: TranscriptEntry = {
      $typeName: "agentfleet.v1.TranscriptEntry",
      sessionId,
      // Sorts after everything real so far; it is never rendered directly
      // (the feed skips both types), only read by the derivations.
      seq: entries.reduce((max, e) => (e.seq > max ? e.seq : max), seq) + 1n,
      from: "human",
      text,
      type,
      replyTo: seq,
      createdAt: "",
    } as TranscriptEntry;

    setOptimistic((prev) => [...prev, echo]);
    return run(action, key).then((ok) => {
      // Rolled back on failure — run() has already surfaced the error, and
      // leaving the echo would show a decision that never reached the fleet.
      if (!ok) setOptimistic((prev) => prev.filter((e) => e !== echo));
    });
  }

  const respondToPermission = (seq: bigint, decision: { behavior: "allow" | "deny"; message?: string }) =>
    decide(
      seq,
      TranscriptEntryType.PERMISSION_RESPONSE,
      JSON.stringify(decision),
      () => client.respondToPermission({ sessionId, seq, decisionJson: JSON.stringify(decision) }),
      `permission:${seq}`,
    );

  const answerQuestion = (seq: bigint, answers: Record<string, string>) =>
    decide(
      seq,
      TranscriptEntryType.ANSWER,
      JSON.stringify({ answers }),
      () => client.answerQuestion({ sessionId, seq, answersJson: JSON.stringify({ answers }) }),
      `question:${seq}`,
    );

  async function sendDiscuss(text: string) {
    pendingRef.current = text;
    setPendingMessage(text);
    setBusyKey("discuss");
    setActionError(null);
    try {
      await client.postMessage({ sessionId, text });
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
    entries: withOptimistic(entries, optimistic),
    branch,
    worktreePath,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    respondToPermission,
    answerQuestion,
    clearActionError: () => setActionError(null),
  };
}
