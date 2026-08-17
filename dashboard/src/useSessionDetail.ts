import { useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, subscribeTranscript } from "./connectClient";
import { withOptimistic } from "./transcript";
import { approvePlan } from "./approvePlan";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// One page of transcript history: what a session opens with, and what one
// click of the feed's "load earlier" header adds. Well under the server's own
// 1000 cap, which clamps anything larger anyway.
const PAGE = 200;

// Shared data-loading for a single session's session view — used by both the
// desktop SessionDetail and the mobile MobileSessionDetail, which differ only in
// layout, not in what they fetch/subscribe to.
export function useSessionDetail(sessionId: string) {
  const [session, setSession] = useState<Session | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  // Both are permanently null: the ListWorktrees lookup that filled them is
  // gone with the worktree model (docs/adr/0048 §5, and see the effect below).
  // Kept as state, and rendered conditionally by both detail views, so
  // restoring them is a matter of writing to them again rather than
  // re-threading two values through both form factors.
  const [branch, setBranch] = useState<string | null>(null);
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
  // without a session), actionError is inline while the loaded view stays up.
  // Collapsing these into one `error` state previously left the error
  // banner unreachable — an unconditional `if (!session) return <Loading/>`
  // ran before it, so any load failure hung on "Loading…" forever.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  // Older history is fetched a page at a time, backwards from the oldest
  // entry currently held. hasOlder drives the feed's "load earlier" header;
  // loadingOlder keeps a double-click from fetching the same page twice.
  const [hasOlder, setHasOlder] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);

  useEffect(() => {
    setSession(null);
    setEntries([]);
    setBranch(null);
    setWorktreePath(null);
    setHasOlder(false);
    setLoadingOlder(false);
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
        setSession(res.session ?? null);
        if (!res.session) return;
        // The branch lookup is gone with ListWorktrees (docs/adr/0048 §5).
        //
        // It read the branch off a WorktreeView keyed by (repo, task_id),
        // which worked because the FLEET named the branch: `agent/<taskId>`,
        // a convention it owned. It no longer creates branches at all — the
        // agent runs `git checkout -b` for whatever it wants — so there is
        // nothing to look up by session id, and guessing would be worse than
        // showing nothing. `changes`, derived from the worker's own telemetry
        // snapshots, is the honest source for what a session is touching.
      })
      .catch((err: ConnectError) => {
        if (cancelled) return;
        setLoadError(err.code === Code.NotFound ? "Session not found." : err.message);
      });

    // The e2e status poll is gone with the sandbox it described
    // (docs/adr/0048 §6). It polled every 5s for a second pod's phase,
    // preview URL and resolved recipe — none of which exist now that the app
    // runs in the session's own pod, started by the agent with Bash.

    let unsubscribe = () => {};
    client
      // The newest page, not the whole transcript. `sinceSeq: 0n` asked for
      // everything and got the server's 1000-entry cap applied to the OLDEST
      // 1000 — so a long session opened on its own beginning, and the entries
      // between #1001 and the stream's start were fetched by nothing, ever.
      .getTranscript({ sessionId, limit: PAGE })
      .then((res) => {
        if (cancelled) return;
        setEntries(res.entries);
        // A full page means there is probably more behind it. Off by at most
        // one empty fetch when the transcript is an exact multiple of PAGE.
        setHasOlder(res.entries.length >= PAGE);
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

  // Fetches the page of entries just before the oldest one held and prepends
  // it. Resolves once state is set, so a caller can anchor the scroll
  // position around the prepend (see useAtBottom's anchorPrepend).
  async function loadOlder(): Promise<void> {
    if (loadingOlder) return;
    const oldest = entries[0]?.seq;
    if (oldest === undefined || oldest <= 0n) {
      setHasOlder(false);
      return;
    }
    setLoadingOlder(true);
    try {
      const res = await client.getTranscript({ sessionId, beforeSeq: oldest, limit: PAGE });
      setEntries((prev) => [...res.entries, ...prev]);
      setHasOlder(res.entries.length >= PAGE);
    } catch (err) {
      setActionError((err as Error).message);
    } finally {
      setLoadingOlder(false);
    }
  }

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

  // Approving a plan may also switch the session's permission mode, in that
  // order (approvePlan.ts) — but it still resolves through decide(), so the
  // optimistic PERMISSION_RESPONSE echo and the rollback-on-failure are the
  // same as any other decision.
  const approvePlanDecision = (seq: bigint, mode?: "auto") =>
    decide(
      seq,
      TranscriptEntryType.PERMISSION_RESPONSE,
      JSON.stringify({ behavior: "allow" }),
      () => approvePlan(sessionId, seq, mode),
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
    session,
    entries: withOptimistic(entries, optimistic),
    branch,
    worktreePath,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    hasOlder,
    loadingOlder,
    loadOlder,
    run,
    sendDiscuss,
    respondToPermission,
    approvePlanDecision,
    answerQuestion,
    clearActionError: () => setActionError(null),
  };
}
