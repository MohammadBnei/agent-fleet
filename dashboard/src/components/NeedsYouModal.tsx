import { useEffect, useRef, useState } from "react";
import {
  answerLine,
  type DeckItem,
  listDecisionHistory,
  withdrawDecision,
} from "../decisions";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "../gen/agentfleet/v1/agent_pb";
import { relativeTime } from "../relativeTime";
import { sessionLabel } from "../sessionLabel";
import { blockedForLabel, shortId } from "../sessionState";
import { ActionButton } from "./ActionButton";
import { DecisionInline } from "./DecisionInline";
import { InlineError } from "./InlineError";
import { Modal } from "./Modal";
import { Segmented } from "./Segmented";

// Every open decision, answerable without leaving where you are.
//
// The header badge used to navigate: it set the sessions view and turned on the
// needs-you filter. From inside a session detail that did nothing visible at
// all (the view was already "sessions", so selectView kept the open session),
// and from anywhere else it threw away whatever you were reading to get to a
// list you then had to click into. A decision is a 5-second interruption; it
// should not cost your place.
//
// Open rows come from the decisions store (useDecisionDeck), one per decision,
// each adapted into the summary DecisionInline already takes — the same
// component the list row renders, so whatever the list can answer, this
// answers the same way. History is read-only and read only while its tab is
// up.

// Words shared with the game island: kind names and state names, as-is.
const KIND: Record<DecisionKind, string> = {
  [DecisionKind.UNSPECIFIED]: "Decision",
  [DecisionKind.QUESTION]: "Question",
  [DecisionKind.PERMISSION]: "Permission",
  [DecisionKind.PLAN]: "Plan",
};
const STATE: Record<DecisionState, string> = {
  [DecisionState.UNSPECIFIED]: "unknown",
  [DecisionState.OPEN]: "open",
  [DecisionState.ANSWERED]: "answered",
  [DecisionState.WITHDRAWN]: "withdrawn",
  [DecisionState.EXPIRED]: "expired",
  [DecisionState.STALE]: "stale",
};

// A fallback item (store unavailable) has no Decision, only the summary.
function itemKind(item: DeckItem): string {
  if (item.decision) return KIND[item.decision.kind];
  const s = item.summary;
  if (s?.pendingQuestion) return "Question";
  if (s?.pendingPermission)
    return s.pendingPermission.tool === "ExitPlanMode" ? "Plan" : "Permission";
  return "Decision";
}

type Tab = "open" | "history";
const TABS: readonly { value: Tab; label: string }[] = [
  { value: "open", label: "Open" },
  { value: "history", label: "History" },
];

export function NeedsYouModal({
  open,
  items,
  available,
  onClose,
  onOpenSession,
  reload,
  initialTab = "open",
  history,
}: {
  open: boolean;
  // The SAME list the badge counts — if the two ever diverge the badge says 3
  // and this renders empty, which reads as the button being broken.
  items: DeckItem[];
  // False while the decisions store refuses this viewer: Open then shows the
  // summaries-derived fallback, and History has nothing to read.
  available: boolean;
  onClose: () => void;
  onOpenSession: (id: string) => void;
  reload: () => void;
  // Tests only: render a tab and its History rows without a fetch (effects
  // never run under renderToStaticMarkup). Production omits both.
  initialTab?: Tab;
  history?: Decision[];
}) {
  const [tab, setTab] = useState<Tab>(initialTab);

  // Answering the last decision closes the sheet. It used to stay up saying
  // "Nothing waiting any more." — the job was done and the human still had to
  // find the way out. Only on a drop to zero while open, and only on the Open
  // tab (someone reading History is not done): opened on an already empty
  // list (the decision resolved between the badge and the click), it stays
  // and says so.
  const lastCount = useRef(items.length);
  useEffect(() => {
    if (open && tab === "open" && lastCount.current > 0 && items.length === 0)
      onClose();
    lastCount.current = items.length;
  }, [open, tab, items.length, onClose]);

  // Nothing at all while closed. Every other Modal in the app renders its
  // children regardless, which is harmless for a form — but this one mirrors
  // every open decision, so a closed dialog kept a second live DecisionInline
  // per decision in the DOM, each with its own state and its own allow/deny
  // buttons. Caught by Playwright resolving two elements for one session
  // label with the dialog shut.
  if (!open) return null;

  return (
    <Modal
      open={open}
      onClose={onClose}
      // Full screen on a phone, as PlanModal is: a bottom sheet capped at
      // 85vh left a plan's actions under the fold behind the sheet's own
      // scroll. The desktop box is unchanged.
      boxClassName="max-w-none w-screen h-[100dvh] max-h-none rounded-none sm:w-auto sm:max-w-3xl sm:h-auto sm:max-h-[85vh] sm:rounded-box overflow-hidden flex flex-col"
    >
      <div className="flex items-center gap-2 mb-3 flex-none">
        <span className="text-2xs tracking-[0.12em] text-error">DECISIONS</span>
        <Segmented value={tab} options={TABS} onChange={setTab} size="sm" />
        <span className="flex-1 h-px bg-pink-line" />
        <span className="text-xs text-dim2">{items.length} open</span>
        <form method="dialog" className="sm:hidden">
          <button
            type="submit"
            aria-label="Close"
            className="text-dim hover:text-base-content px-1 cursor-pointer"
          >
            ✕
          </button>
        </form>
      </div>
      {/* min-h-0 or the column refuses to scroll and the sheet grows instead. */}
      <div className="flex flex-col gap-4 overflow-y-auto min-h-0">
        {tab === "history" ? (
          available ? (
            <History onOpenSession={onOpenSession} initial={history} />
          ) : (
            <div className="text-sm text-dim2">
              The decision history is unavailable here.
            </div>
          )
        ) : items.length === 0 ? (
          // Reachable for real: a decision can resolve between the update
          // that drew the badge and the click on it.
          <div className="text-sm text-dim2">No decisions waiting.</div>
        ) : (
          items.map((item) => (
            <OpenRow
              key={item.key}
              item={item}
              onOpenSession={onOpenSession}
              reload={reload}
            />
          ))
        )}
      </div>
    </Modal>
  );
}

function OpenRow({
  item,
  onOpenSession,
  reload,
}: {
  item: DeckItem;
  onOpenSession: (id: string) => void;
  reload: () => void;
}) {
  const { session: s, summary, decision: d } = item;
  const age = d
    ? relativeTime(d.createdAt, { bare: true })
    : blockedForLabel(s);
  const queued = summary?.pendingPermissionCount ?? 0;
  const permission = summary?.pendingPermission;
  // Withdraw only where nothing is waiting on the answer: a stamped request
  // (its worker already moved on) or an interrupted one (the InterruptedBar
  // inside DecisionInline carries that close). Withdrawing a live unstamped
  // request leaves the old worker's canUseTool hanging on a reply that never
  // comes.
  const canWithdraw =
    d !== undefined &&
    d.kind !== DecisionKind.QUESTION &&
    permission?.stamped === true &&
    !permission.interrupted;
  return (
    <div className="border border-pink-line">
      <div className="flex items-baseline gap-2 px-3.5 pt-3 min-w-0">
        <span className="text-xs text-error tracking-[0.05em] flex-none">
          {itemKind(item)} · waiting on you
        </span>
        {age && <span className="text-xs text-dim2 flex-none">{age}</span>}
        {canWithdraw && permission && (
          <WithdrawButton
            sessionId={s.id}
            seq={permission.entry.seq}
            reload={reload}
          />
        )}
      </div>
      {/* Title first, id demoted to the meta line — same ordering as the list
          rows and the actions modal, so the thing you scan for is always in
          the same place. */}
      <div className="flex items-baseline gap-2 px-3.5 pt-1 min-w-0">
        <button
          type="button"
          onClick={() => onOpenSession(s.id)}
          className="text-sm font-semibold min-w-0 truncate text-left hover:text-primary cursor-pointer flex-1"
        >
          {sessionLabel(s)}
        </button>
        <span className="text-xs text-dim2 flex-none">
          {s.repo}
          {queued > 1 ? ` · ${queued} decisions` : ""}
        </span>
      </div>
      <div className="px-3.5 pt-0.5 text-2xs text-dim2">#{shortId(s.id)}</div>
      <DecisionInline
        session={s}
        summary={summary}
        reload={reload}
        claimLabel
      />
    </div>
  );
}

function WithdrawButton({
  sessionId,
  seq,
  reload,
}: {
  sessionId: string;
  seq: bigint;
  reload: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <span className="ml-auto flex-none flex items-baseline gap-2">
      <InlineError message={error} />
      <ActionButton
        busy={busy}
        disabled={busy}
        title="Close this request without answering it"
        className="text-xs text-dim hover:text-error cursor-pointer border border-line px-2 py-0.5"
        onClick={() => {
          setBusy(true);
          setError(null);
          withdrawDecision(sessionId, seq)
            .then(reload)
            .catch((err: Error) => setError(err.message))
            .finally(() => setBusy(false));
        }}
      >
        withdraw
      </ActionButton>
    </span>
  );
}

// Mounted only while the History tab is up, so the store is read when someone
// looks and a reopened sheet starts from the newest page again.
function History({
  onOpenSession,
  initial,
}: {
  onOpenSession: (id: string) => void;
  initial?: Decision[];
}) {
  const [rows, setRows] = useState<Decision[]>(initial ?? []);
  // "" is the first page; null means there is no further page.
  const [token, setToken] = useState<string | null>(initial ? null : "");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadPage = (pageToken: string) => {
    setLoading(true);
    setError(null);
    listDecisionHistory(pageToken)
      .then((res) => {
        setRows((prev) => [...prev, ...res.decisions]);
        setToken(res.nextPageToken || null);
      })
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false));
  };

  // First page once, on mount, unless handed one.
  useEffect(() => {
    if (!initial) loadPage("");
  }, []);

  return (
    <div className="flex flex-col">
      {rows.map((d) => (
        <div
          key={d.id}
          className="flex items-baseline gap-2 py-2 border-b border-line min-w-0 text-sm"
        >
          <span className="flex-none text-xs">{KIND[d.kind]}</span>
          <span className="flex-none text-2xs text-dim2 border border-line px-1">
            {STATE[d.state]}
          </span>
          <span className="flex-1 min-w-0 truncate text-text2">
            {answerLine(d)}
          </span>
          <span className="flex-none text-xs text-dim2">
            {relativeTime(d.answeredAt || d.createdAt) ?? ""}
          </span>
          <button
            type="button"
            onClick={() => onOpenSession(d.threadId)}
            title={d.subjectRepo || undefined}
            className="flex-none text-xs text-dim hover:text-primary cursor-pointer"
          >
            #{shortId(d.threadId)}
          </button>
        </div>
      ))}
      <InlineError message={error} />
      {!loading && !error && rows.length === 0 && token === null && (
        <div className="text-sm text-dim2">
          No decisions in the history yet.
        </div>
      )}
      {loading ? (
        <div className="py-2 text-xs text-dim2">loading…</div>
      ) : (
        token !== null &&
        token !== "" && (
          <button
            type="button"
            onClick={() => loadPage(token)}
            className="py-2 text-xs text-dim hover:text-primary cursor-pointer self-start"
          >
            older ▸
          </button>
        )
      )}
    </div>
  );
}
