import { Code, type ConnectError } from "@connectrpc/connect";
import { useCallback, useEffect, useRef, useState } from "react";
import { errorText } from "./agentErrors";
import { approvePlan } from "./approvePlan";
import type { Grant, GrantPreview } from "./components/PermissionCard";
import { client, subscribeTranscript } from "./connectClient";
import { fetchWithdrawn, withdrawDecision, withdrawnReply } from "./decisions";
import type { Session } from "./gen/agentfleet/v1/core_pb";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "./gen/agentfleet/v1/transcript_pb";
import { pollVisible } from "./pollVisible";
import { queueFor } from "./serialQueue";
import { keysOf, scopeKey } from "./sessionScope";
import { isPodPhaseLive } from "./sessionState";
import { withOptimistic } from "./transcript";
import { useBusyAction } from "./useBusyAction";

// The first message of a brand-new session, handed over by NewSessionDialog so
// the dialog can close the instant the session row exists.
//
// Module-level rather than a prop, and deleted before it is sent. Threading it
// through App into two detail views would need a "did I already send this?"
// ref, and a ref is not enough: StrictMode double-invokes effects, AND the
// desktop/mobile switch unmounts one detail view to mount the other, which
// resets any per-component guard. The server cannot dedupe either — Append
// mints a fresh idempotency key per call (docs/adr/0013) — so a double send is
// two real messages to the agent.
//
// Delete-before-send makes the second caller find nothing, whichever way it
// arrived.
const firstMessages = new Map<string, string>();

export function queueFirstMessage(sessionId: string, text: string) {
  if (text.trim()) firstMessages.set(sessionId, text);
}

// One page of transcript history: what a session opens with, and what one
// click of the feed's "load earlier" header adds. Well under the server's own
// 1000 cap, which clamps anything larger anyway.
const PAGE = 200;

// Same cadence as App's list poll.
const SESSION_POLL_MS = 5000;

// Shared data-loading for a single session's session view — used by both the
// desktop SessionDetail and the mobile MobileSessionDetail, which differ only in
// layout, not in what they fetch/subscribe to.
//
// `listed` is App's polled copy of this session, when the list has it. The
// one-shot getSession below used to be the ONLY copy on mobile, so pod phase,
// mode and live state froze at whatever they were when the session opened.
export function useSessionDetail(sessionId: string, listed?: Session) {
  const [session, setSession] = useState<Session | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  // Keyed, not a plain boolean — a click on one PermissionCard/PlanCard used
  // to disable every other pending card and the whole ActionsMenu at once.
  // Each caller passes its own key to run() (a permission/question's own
  // seq, or a fixed string for ActionsMenu's page-level actions, which
  // still disable together on purpose); "actions" callers still tie
  // themselves to busyKey !== null, not a specific key.
  //
  // busyKey, actionError and run() all come from useBusyAction — the same three
  // things SessionActionsModal needs for a list row, and they were written out
  // twice with different field names for the same state.
  // setActionError is useBusyAction's useState setter and therefore stable, but
  // the linter cannot see through the hook boundary — so it is listed in the
  // reset effect's deps below rather than the rule being silenced.
  const {
    busyKeys: allBusyKeys,
    error: actionError,
    run: runKeyed,
    setError: setActionError,
  } = useBusyAction();
  // Which session this hook is on RIGHT NOW. Work started under another one
  // finishes into the void: its error must not open on this session, and its
  // keys must not read as this session's busy buttons (keys are scoped below,
  // because permission:${seq} and question:${seq} repeat across sessions).
  const idRef = useRef(sessionId);
  idRef.current = sessionId;
  const busyKeys = keysOf(sessionId, allBusyKeys);
  const busyKey = busyKeys.length > 0 ? busyKeys[busyKeys.length - 1] : null;
  const isBusy = (key: string) => busyKeys.includes(key);
  // Warm-capable calls (a message, an answer, the Warm button) take seconds on
  // a cold session and the server does not order two of them (see
  // serialQueue). They wait here instead of locking the page; `starting` says
  // how many of them are waiting on a cold pod, for the small indicator.
  const [starting, setStarting] = useState(0);
  const sessionRef = useRef<Session | null>(null);
  // Optimistic echo for a just-sent human message — client.postMessage() plus
  // the streamTranscript round trip to see it reflected back is enough
  // latency to feel laggy, so this shows the message immediately with a
  // spinner instead of waiting. Cleared once the real entry arrives (ref,
  // not state, so the subscribeTranscript closure below always reads the
  // latest value without needing to resubscribe on every keystroke/send).
  // A list: sends queue behind a warm, and each shows until its own echo lands.
  const [pendingMessages, setPendingMessages] = useState<{ text: string }[]>(
    [],
  );
  const pendingRef = useRef<{ text: string }[]>([]);
  const setPending = useCallback(
    (fn: (prev: { text: string }[]) => { text: string }[]) => {
      pendingRef.current = fn(pendingRef.current);
      setPendingMessages(pendingRef.current);
    },
    [],
  );
  // The same idea as pendingMessage, for decisions: answering a permission
  // is two round trips (the RPC, then the response entry arriving on the
  // stream) before the card stops offering allow/deny. Holding the answer
  // here and merging it into `entries` means every surface that derives
  // from entries — feed card, dock, list row — agrees instantly, and the
  // real entry supersedes it the moment it lands. See withOptimistic.
  const [optimistic, setOptimistic] = useState<TranscriptEntry[]>([]);
  // Requests closed in the decisions store, which leaves no transcript row
  // (decisions.ts). Read once per open, and echoed on this view's own close.
  // ponytail: a close from another surface shows on the next open; watch
  // WatchDecisions when that lag matters.
  const [withdrawn, setWithdrawn] = useState<TranscriptEntry[]>([]);
  // Two states, not one: loadError blocks rendering (nothing to show
  // without a session), actionError (from useBusyAction) is inline while the
  // loaded view stays up.
  // Collapsing these into one `error` state previously left the error
  // banner unreachable — an unconditional `if (!session) return <Loading/>`
  // ran before it, so any load failure hung on "Loading…" forever.
  const [loadError, setLoadError] = useState<string | null>(null);
  // Older history is fetched a page at a time, backwards from the oldest
  // entry currently held. hasOlder drives the feed's "load earlier" header;
  // loadingOlder keeps a double-click from fetching the same page twice.
  const [hasOlder, setHasOlder] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);

  useEffect(() => {
    setSession(null);
    setEntries([]);
    setHasOlder(false);
    setLoadingOlder(false);
    setLoadError(null);
    setActionError(null);
    setPending(() => []);
    setOptimistic([]);
    setStarting(0);
    setWithdrawn([]);

    let cancelled = false;
    fetchWithdrawn(sessionId).then((w) => !cancelled && setWithdrawn(w));
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
        // The worktree-path lookup is gone with ListWorktrees (docs/adr/0048
        // §5). It read off a WorktreeView keyed by (repo, task_id), which
        // worked because the FLEET named the branch: `agent/<taskId>`, a
        // convention it owned. It no longer creates branches at all — the
        // agent runs `git checkout -b` for whatever it wants — so there is
        // nothing to look up by session id. `changes` and `branch`, derived
        // from the worker's own telemetry snapshots, are the honest source for
        // what a session is touching.
      })
      .catch((err: ConnectError) => {
        if (cancelled) return;
        setLoadError(
          err.code === Code.NotFound ? "Session not found." : err.message,
        );
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
          if (entry.from === "human") {
            const at = pendingRef.current.findIndex(
              (p) => p.text === entry.text,
            );
            if (at >= 0) setPending((prev) => prev.filter((_, i) => i !== at));
          }
        });
      })
      .catch((err: Error) => !cancelled && setLoadError(err.message));

    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, [sessionId, setActionError, setPending]);

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
      const res = await client.getTranscript({
        sessionId,
        beforeSeq: oldest,
        limit: PAGE,
      });
      setEntries((prev) => [...res.entries, ...prev]);
      setHasOlder(res.entries.length >= PAGE);
    } catch (err) {
      setActionError(errorText(err));
    } finally {
      setLoadingOlder(false);
    }
  }

  const queued = useCallback(
    <T>(action: () => Promise<T>): Promise<T> => {
      const cold = !isPodPhaseLive(sessionRef.current?.podPhase);
      const bump = (d: number) => {
        if (cold && idRef.current === sessionId) setStarting((n) => n + d);
      };
      bump(1);
      return queueFor(sessionId)(action).finally(() => bump(-1));
    },
    [sessionId],
  );

  // Warm is queued like a send; everything else runs straight away. Keys are
  // scoped to the session, and a failure that lands after the user has moved
  // to another session is dropped rather than shown there.
  const run = useCallback(
    (action: () => Promise<unknown>, key: string) => {
      const job = key === "action:warm" ? () => queued(action) : action;
      return runKeyed(
        async () => {
          try {
            await job();
          } catch (err) {
            if (idRef.current === sessionId) throw err;
          }
        },
        scopeKey(sessionId, key),
      );
    },
    [runKeyed, queued, sessionId],
  );

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

  const respondToPermission = (
    seq: bigint,
    decision: { behavior: "allow" | "deny"; message?: string },
    grant?: Grant,
  ) =>
    decide(
      seq,
      TranscriptEntryType.PERMISSION_RESPONSE,
      JSON.stringify(decision),
      () =>
        client.respondToPermission({
          sessionId,
          seq,
          decisionJson: JSON.stringify(decision),
          scope: grant?.scope,
          ruleForm: grant?.ruleForm,
        }),
      `permission:${seq}`,
    );

  const previewGrant: GrantPreview = useCallback(
    (seq: bigint) =>
      client
        .previewGrant({ sessionId, seq })
        .then((r) => ({ exact: r.exact, prefix: r.prefix })),
    [sessionId],
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
      // Answering a cold session warms it, so it waits its turn like a send.
      () =>
        queued(() =>
          client.answerQuestion({
            sessionId,
            seq,
            answersJson: JSON.stringify({ answers }),
          }),
        ),
      `question:${seq}`,
    );

  // Closes an interrupted request unanswered. No optimistic echo through
  // decide(): nothing real ever supersedes a withdraw, so the closure is held
  // here instead, once core has confirmed it.
  const withdraw = (seq: bigint) =>
    run(() => withdrawDecision(sessionId, seq), `withdraw:${seq}`).then(
      (ok) =>
        ok && setWithdrawn((prev) => [...prev, withdrawnReply(sessionId, seq)]),
    );

  // Through run() like everything else, with one extra job: roll back the
  // optimistic echo when the post fails, so a message that never left does not
  // sit in the feed looking sent.
  // Resolves to false when the message did not go, so the composer can put
  // its text back.
  const sendDiscuss = useCallback(
    async (text: string): Promise<boolean> => {
      // Not through run(): that would hold busyKey, and with it the composer
      // and the menu, for the whole warm. The echo is the feedback; the queue
      // keeps order; the composer clears on send, so no double-submit.
      const item = { text };
      setPending((prev) => [...prev, item]);
      setActionError(null);
      try {
        await queued(() => client.postMessage({ sessionId, text }));
        return true;
      } catch (err) {
        // Gone to another session meanwhile: nothing here is theirs now.
        if (idRef.current !== sessionId) return true;
        setActionError(errorText(err));
        setPending((prev) => prev.filter((p) => p !== item));
        return false;
      }
    },
    [sessionId, queued, setPending, setActionError],
  );

  // A brand-new session's first message, if NewSessionDialog left one here.
  // Its own effect, not folded into the session load above, so it can depend
  // on sendDiscuss honestly rather than silencing exhaustive-deps.
  //
  // The dialog hands the text over instead of awaiting postMessage, because
  // postMessage IS the pod boot — clone, fleet-shared sync, PVC — and blocking
  // a modal on it left a human watching a spinner for all of it. Routing
  // through sendDiscuss gives the first message the same optimistic echo and
  // the same inline error as every other message.
  // When the list does not carry this session (a search that excludes it, a
  // deep link to one the list does not return), poll it here instead — the
  // fallback copy was otherwise frozen at open. A failed refresh keeps the
  // last good session on screen: one dropped poll must not swap a working
  // view for an error banner. A real disappearance (delete) still surfaces,
  // through the list and the transcript stream.
  const hasListed = listed !== undefined;
  useEffect(() => {
    if (hasListed) return;
    let cancelled = false;
    const stop = pollVisible(() => {
      client
        .getSession({ id: sessionId })
        .then((res) => !cancelled && res.session && setSession(res.session))
        .catch(() => {});
    }, SESSION_POLL_MS);
    return () => {
      cancelled = true;
      stop();
    };
  }, [sessionId, hasListed]);

  useEffect(() => {
    const queued = firstMessages.get(sessionId);
    if (queued === undefined) return;
    // Delete BEFORE sending: StrictMode double-invokes effects, and the
    // desktop/mobile switch remounts this hook entirely. Whichever call
    // arrives second must find nothing.
    firstMessages.delete(sessionId);
    void sendDiscuss(queued);
  }, [sessionId, sendDiscuss]);

  const current = session && (listed ?? session);
  sessionRef.current = current;
  return {
    // Prefer the list's copy, but only once the one-shot fetch has landed, so
    // `session === null` still means "loading" to both views.
    session: current,
    // Withdrawn closures first: every derivation correlates by replyTo, and
    // anything positional (the newest entry, loadOlder's oldest) still sees
    // only real rows at the ends that matter.
    entries: withOptimistic(
      withdrawn.length === 0 ? entries : [...withdrawn, ...entries],
      optimistic,
    ),
    busyKey,
    isBusy,
    // Page-level disabling is for the menu's own actions only; a card's answer
    // or a queued send must not lock unrelated buttons.
    menuBusy:
      busyKeys.some((k) => k.startsWith("action:")) ||
      pendingMessages.some((p) => p.text === "/clear"),
    startingPod: starting > 0,
    loadError,
    actionError,
    pendingMessages: pendingMessages.map((p) => p.text),
    hasOlder,
    loadingOlder,
    loadOlder,
    run,
    sendDiscuss,
    respondToPermission,
    // Only a definition session remembers approvals (docs/adr/0076 §3).
    previewGrant: session?.definitionName ? previewGrant : undefined,
    approvePlanDecision,
    answerQuestion,
    withdraw,
    clearActionError: () => setActionError(null),
  };
}
