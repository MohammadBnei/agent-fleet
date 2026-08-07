import { useState } from "react";
import { Markdown } from "./Markdown";

// ExitPlanMode's canUseTool call posts a real, seq-correlated
// PERMISSION_REQUEST entry like any other tool (see transcript.ts's
// findPendingPermissions) — "Approve" here just calls RespondToPermission
// with an allow decision. "Request changes" reuses the existing
// sendDiscuss flow: worker/src/session.ts already treats any human reply
// while a permission request is pending as deny-with-feedback, re-arming
// the agent to call ExitPlanMode again with a revised plan.
//
// `edgeClassName` breaks the card out of the feed's own horizontal padding
// so it can span full width — desktop/mobile pass their own negative-margin
// pair since their feed containers use different padding.
export function PlanCard({
  plan,
  pending,
  busy,
  onApprove,
  onFeedback,
  edgeClassName,
}: {
  plan: string;
  pending: boolean;
  busy: boolean;
  onApprove: () => void;
  onFeedback: (text: string) => void;
  edgeClassName: string;
}) {
  const [feedbackOpen, setFeedbackOpen] = useState(false);
  const [feedback, setFeedback] = useState("");

  if (!pending) {
    return (
      <div className="flex items-center gap-1.5 text-[10.5px] text-base-content/40">
        <span className="badge badge-ghost badge-xs">plan</span>
        <span className="truncate">{plan.split("\n")[0]}</span>
      </div>
    );
  }

  return (
    <div className={`border-y-2 border-primary/40 bg-primary/[0.04] py-4 ${edgeClassName}`}>
      <div className="text-[10px] tracking-[0.12em] font-semibold text-primary mb-2">PLAN — NEEDS YOUR REVIEW</div>
      <div className="text-[12px] leading-relaxed text-base-content/90 max-h-[50vh] overflow-y-auto">
        <Markdown text={plan} />
      </div>
      <div className="flex items-center gap-2 mt-3">
        <button type="button" className="btn btn-success btn-sm" disabled={busy} onClick={onApprove}>
          Approve
        </button>
        <button
          type="button"
          className="btn btn-outline btn-sm"
          disabled={busy}
          onClick={() => setFeedbackOpen((v) => !v)}
        >
          Request changes
        </button>
      </div>
      {feedbackOpen && (
        <div className="flex items-center gap-2 mt-2">
          <input
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && feedback.trim()) {
                onFeedback(feedback);
                setFeedback("");
                setFeedbackOpen(false);
              }
            }}
            placeholder="what should change?"
            autoFocus
            disabled={busy}
            className="flex-1 bg-transparent border border-base-content/15 rounded-lg px-3 py-2 text-[12px] outline-none focus:border-primary/50"
          />
          <button
            type="button"
            className="btn btn-primary btn-sm"
            disabled={busy || !feedback.trim()}
            onClick={() => {
              onFeedback(feedback);
              setFeedback("");
              setFeedbackOpen(false);
            }}
          >
            Send
          </button>
        </div>
      )}
    </div>
  );
}
