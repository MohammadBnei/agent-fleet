import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  decisionAnswerable,
  findPendingPermissions,
  findPendingQuestion,
  hasPendingDecision,
} from "../transcript";
import { ActionButton } from "./ActionButton";
import {
  type Grant,
  type GrantPreview,
  PermissionCard,
} from "./PermissionCard";
import { PlanCard } from "./PlanCard";
import { QuestionCard } from "./QuestionCard";

// The pending decision, pinned rather than inline — the single place either
// form factor answers from. Both detail views pass `dockPendingDecision` to
// SessionFeed so the feed skips the same entry; rendering it in two places at
// once was the redundancy this replaces (docs/adr/0043).
//
// It renders the *same* PlanCard/PermissionCard/QuestionCard the feed uses for
// resolved decisions, which is the point: mobile used to hand-roll its own
// flattened copy of all three, so a multi-select question had nowhere to dock
// and a denial had no reason field on a phone. The cards bring their own pink
// chrome, so this wrapper is only the pin, the scroll bound, and the edge.
export function DecisionDock({
  entries,
  busyKey,
  // The session's pod state, for the permission rule below. Optional so a
  // caller with no session in hand still renders — it defaults to "live",
  // which is the pre-existing behaviour.
  podLive = true,
  swept = false,
  compact = false,
  onRespond,
  previewGrant,
  onApprovePlan,
  onAnswer,
  onPlanFeedback,
  onWithdraw,
}: {
  entries: TranscriptEntry[];
  busyKey: string | null;
  podLive?: boolean;
  swept?: boolean;
  compact?: boolean;
  onRespond: (
    seq: bigint,
    decision: { behavior: "allow" | "deny"; message?: string },
    grant?: Grant,
  ) => void;
  previewGrant?: GrantPreview;
  // Plan approval is its own path, not an allow decision: it may also switch
  // the session's permission mode, and the order of those two calls matters
  // (approvePlan.ts).
  onApprovePlan: (seq: bigint, mode?: "auto") => void;
  onAnswer: (seq: bigint, answers: Record<string, string>) => void;
  onPlanFeedback: (text: string) => void;
  // AnswerDecision "withdraw" (docs/adr/0082 §5): closes an interrupted
  // request unanswered, with no warm and no message.
  onWithdraw?: (seq: bigint) => void;
}) {
  if (!hasPendingDecision(entries)) return null;

  // A permission blocks the agent's own tool call, so it outranks a question
  // when both are somehow open.
  const permissions = findPendingPermissions(entries);
  const permission = permissions[0] ?? null;
  const question = permission ? null : findPendingQuestion(entries);
  // Same rule as the list (see decisionAnswerable): an old worker's permission
  // dies with the pod that raised it; a stamped one and a question do not.
  // Offering allow/deny here on a dead pod is the detail view telling a
  // different story from the list about the same session.
  const permissionLive =
    permission !== null &&
    decisionAnswerable("permission", {
      podLive,
      swept,
      stamped: permission.stamped,
    });
  const pad = compact ? "px-3.5 py-3" : "px-4.5 py-3.5";
  const edge = compact ? "-mx-3.5 px-3.5" : "-mx-4.5 px-4.5";

  return (
    <div
      className={`flex-none border-t border-pink-line max-h-[45vh] overflow-y-auto ${pad}`}
    >
      {/* Parallel tool calls each get their own permission, so several can be
          open at once — and answering one swaps a different card into the same
          pixels with nothing to say the queue moved. Without this line the
          human cannot tell whether they just finished or are on a treadmill. */}
      {permissions.length > 1 && (
        <div className="text-2xs tracking-[0.1em] text-dim2 mb-2">
          PERMISSION 1 OF {permissions.length} · {permissions.length - 1} MORE
          AFTER THIS
        </div>
      )}
      {permission && !permissionLive && (
        <div className="flex flex-col gap-2">
          <PermissionCard
            tool={permission.tool}
            input={permission.input}
            pending={false}
            busy
            onAllow={() => {}}
            onDeny={() => {}}
            edgeClassName={edge}
          />
          <span className="text-xs text-warning">
            {swept
              ? "This session's working directory was reclaimed — the request can no longer be answered."
              : "The pod that asked this is gone, so allow/deny would reach nothing. Warm the session to carry on."}
          </span>
        </div>
      )}
      {permission && permissionLive && permission.interrupted && (
        <InterruptedBar
          busy={busyKey === `withdraw:${permission.entry.seq}`}
          onClose={onWithdraw && (() => onWithdraw(permission.entry.seq))}
        />
      )}
      {permission &&
        permissionLive &&
        (permission.tool === "ExitPlanMode" ? (
          <PlanCard
            plan={
              (permission.input as { plan?: string } | undefined)?.plan ?? ""
            }
            pending
            docked
            busy={busyKey === `permission:${permission.entry.seq}`}
            onApprove={(mode) => onApprovePlan(permission.entry.seq, mode)}
            onFeedback={onPlanFeedback}
            edgeClassName={edge}
          />
        ) : (
          <PermissionCard
            key={String(permission.entry.seq)}
            tool={permission.tool}
            input={permission.input}
            pending
            busy={busyKey === `permission:${permission.entry.seq}`}
            onAllow={(grant) =>
              onRespond(permission.entry.seq, { behavior: "allow" }, grant)
            }
            preview={previewGrant && (() => previewGrant(permission.entry.seq))}
            onDeny={(message) =>
              onRespond(permission.entry.seq, { behavior: "deny", message })
            }
            edgeClassName={edge}
          />
        ))}
      {question && (
        <QuestionCard
          entry={question}
          answer={null}
          busy={busyKey === `question:${question.seq}`}
          compact={compact}
          onSubmit={(answers) => onAnswer(question.seq, answers)}
        />
      )}
    </div>
  );
}

// The turn that asked has ended (an interrupt or a stop after the request).
// The request is still open and still answerable — an answer reaches the agent
// as a message — but it may no longer matter, so the human can also just close
// it. Exported for the list row, which shows the same bar.
export function InterruptedBar({
  busy,
  onClose,
}: {
  busy: boolean;
  onClose?: () => void;
}) {
  return (
    <div className="flex items-center gap-2 mb-2 text-xs text-warning">
      <span className="border border-pink-line px-1 tracking-[0.05em]">
        interrupted
      </span>
      <span className="text-dim">the turn that asked has ended</span>
      {onClose && (
        <ActionButton
          busy={busy}
          disabled={busy}
          title="Close this request without answering it"
          className="ml-auto text-xs text-dim hover:text-error cursor-pointer border border-line px-2 py-0.5"
          onClick={onClose}
        >
          close
        </ActionButton>
      )}
    </div>
  );
}
