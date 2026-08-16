import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { findPendingPermissions, findPendingQuestion, hasPendingDecision } from "../transcript";
import { PermissionCard } from "./PermissionCard";
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
  compact = false,
  onRespond,
  onApprovePlan,
  onAnswer,
  onPlanFeedback,
}: {
  entries: TranscriptEntry[];
  busyKey: string | null;
  compact?: boolean;
  onRespond: (seq: bigint, decision: { behavior: "allow" | "deny"; message?: string }) => void;
  // Plan approval is its own path, not an allow decision: it may also switch
  // the session's permission mode, and the order of those two calls matters
  // (approvePlan.ts).
  onApprovePlan: (seq: bigint, mode?: "auto") => void;
  onAnswer: (seq: bigint, answers: Record<string, string>) => void;
  onPlanFeedback: (text: string) => void;
}) {
  if (!hasPendingDecision(entries)) return null;

  // A permission blocks the agent's own tool call, so it outranks a question
  // when both are somehow open.
  const permission = findPendingPermissions(entries)[0] ?? null;
  const question = permission ? null : findPendingQuestion(entries);
  const pad = compact ? "px-3.5 py-3" : "px-4.5 py-3.5";
  const edge = compact ? "-mx-3.5 px-3.5" : "-mx-4.5 px-4.5";

  return (
    <div className={`flex-none border-t border-pink-line max-h-[45vh] overflow-y-auto ${pad}`}>
      {permission &&
        (permission.tool === "ExitPlanMode" ? (
          <PlanCard
            plan={(permission.input as { plan?: string } | undefined)?.plan ?? ""}
            pending
            docked
            busy={busyKey === `permission:${permission.entry.seq}`}
            onApprove={(mode) => onApprovePlan(permission.entry.seq, mode)}
            onFeedback={onPlanFeedback}
            edgeClassName={edge}
          />
        ) : (
          <PermissionCard
            tool={permission.tool}
            input={permission.input}
            pending
            busy={busyKey === `permission:${permission.entry.seq}`}
            onAllow={() => onRespond(permission.entry.seq, { behavior: "allow" })}
            onDeny={(message) => onRespond(permission.entry.seq, { behavior: "deny", message })}
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
