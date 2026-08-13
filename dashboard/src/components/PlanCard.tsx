import { useRef, useState } from "react";
import { Markdown } from "./Markdown";
import { ActionButton } from "./ActionButton";

type Annotation = { quote: string; comment: string };
type SelectionPopover = { quote: string; x: number; y: number; editing: boolean };

// ExitPlanMode's canUseTool call posts a real, seq-correlated
// PERMISSION_REQUEST entry like any other tool (see transcript.ts's
// findPendingPermissions) — "Approve" here just calls RespondToPermission
// with an allow decision. "Request changes" reuses the existing
// sendDiscuss flow: worker/src/session.ts already treats any human reply
// while a permission request is pending as deny-with-feedback, re-arming
// the agent to call ExitPlanMode again with a revised plan.
//
// allowAnnotate (desktop only, see call sites) adds plannotator-style
// passage-level annotation on top of that: select text in the rendered
// plan, attach a comment, collect several, and they get folded into one
// structured message on send — still just a string through the same
// onFeedback prop, so nothing downstream changes. Selected text is
// captured as a quote only, not highlighted in place — react-markdown
// re-renders from source, so mapping a DOM Range back to a markdown
// offset to keep a highlight anchored would be real, fragile work for
// what a quoted excerpt already conveys.
//
// `edgeClassName` breaks the card out of the feed's own horizontal padding
// so it can span full width — desktop/mobile pass their own negative-margin
// pair since their feed containers use different padding.
export function PlanCard({
  plan,
  pending,
  busy,
  decision,
  onApprove,
  onFeedback,
  edgeClassName,
  allowAnnotate = true,
  docked = false,
}: {
  plan: string;
  pending: boolean;
  busy: boolean;
  // The dock already caps its own height and scrolls (that is the whole of
  // what that wrapper does), so the plan body must NOT bring a second
  // scroller inside it. Nested, the inner box was taller than the dock
  // itself — 422px of plan inside a 379px dock on a phone — so dragging the
  // plan text only ever moved the inner box, and Approve / request changes
  // sat permanently below the fold with no way to drag to them.
  docked?: boolean;
  // "Request changes" never resolves the request itself (see this file's
  // own top comment) — it stays pending and re-arms via a plain reply, so
  // the only way `!pending` happens here without a real Approve is a later
  // Kill/Interrupt sweeping it up unanswered (transcript.ts's
  // resolvedPermissionDecisions). Undefined (the pre-fix legacy case) keeps
  // the old unconditional "plan approved" fallback.
  decision?: "allow" | "deny" | "interrupted";
  onApprove: () => void;
  onFeedback: (text: string) => void;
  edgeClassName: string;
  allowAnnotate?: boolean;
}) {
  const [feedbackOpen, setFeedbackOpen] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [annotations, setAnnotations] = useState<Annotation[]>([]);
  const [selection, setSelection] = useState<SelectionPopover | null>(null);
  const [draft, setDraft] = useState("");
  const [isExpanded, setIsExpanded] = useState(false);
  const wrapperRef = useRef<HTMLDivElement>(null);

  if (!pending) {
    return (
      <button
        type="button"
        onClick={() => setIsExpanded(!isExpanded)}
        className="flex items-start gap-2 text-xs text-dim2 hover:text-dim w-full text-left group"
      >
        <span className={`flex-none border px-1 ${decision === "interrupted" ? "border-pink-line text-error" : "border-green-line text-green-soft"}`}>{decision === "interrupted" ? "plan interrupted" : "plan approved"}</span>
        {isExpanded ? (
          <div className="flex-1 min-w-0">
            <Markdown text={plan} />
          </div>
        ) : (
          <div className="truncate flex-1">
            <Markdown text={plan.split("\n")[0]} />
          </div>
        )}
        <span className="text-2xs flex-none group-hover:text-dim">
          {isExpanded ? "▴" : "▾"}
        </span>
      </button>
    );
  }

  function handleMouseUp() {
    if (!allowAnnotate) return;
    const sel = window.getSelection();
    const text = sel?.toString().trim();
    // A plain click (no drag) collapses the selection — dismiss a stale
    // popover, but never while the user is mid-edit of one (the comment
    // input lives outside this handler's element, so it can't be the
    // click that triggered this).
    if (!text || !sel || sel.rangeCount === 0) {
      setSelection((s) => (s?.editing ? s : null));
      return;
    }
    const wrapper = wrapperRef.current;
    if (!wrapper) return;
    const rect = sel.getRangeAt(0).getBoundingClientRect();
    const wrapperRect = wrapper.getBoundingClientRect();
    setSelection({
      quote: text,
      x: rect.left - wrapperRect.left + rect.width / 2,
      y: rect.top - wrapperRect.top,
      editing: false,
    });
    setDraft("");
  }

  function addAnnotation() {
    if (!selection || !draft.trim()) return;
    setAnnotations((prev) => [...prev, { quote: selection.quote, comment: draft.trim() }]);
    setSelection(null);
    setDraft("");
    window.getSelection()?.removeAllRanges();
  }

  function removeAnnotation(index: number) {
    setAnnotations((prev) => prev.filter((_, i) => i !== index));
  }

  // Zero annotations sends exactly what's typed, unchanged from before this
  // feature existed. One or more folds them into a numbered list ahead of
  // the free-text box's contents, now framed as an overall comment.
  function send() {
    const overall = feedback.trim();
    const text =
      annotations.length === 0
        ? overall
        : `Requested changes:\n${annotations
            .map((a, i) => `${i + 1}. On: "${a.quote}"\n   → ${a.comment}`)
            .join("\n")}${overall ? `\n\n${overall}` : ""}`;
    if (!text) return;
    onFeedback(text);
    setFeedback("");
    setAnnotations([]);
    setFeedbackOpen(false);
  }

  return (
    <div className={`border-y border-pink-line bg-pink-bg py-4 ${edgeClassName}`}>
      <div className="flex items-center gap-2 mb-2.5">
        <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse flex-none" />
        <span className="text-2xs tracking-[0.1em] text-error">◉ PLAN — NEEDS YOUR REVIEW</span>
      </div>
      <div ref={wrapperRef} className="relative">
        <div
          onMouseUp={handleMouseUp}
          className={`text-base leading-[1.7] ${docked ? "" : "max-h-[50vh] overflow-y-auto"}`}
        >
          <Markdown text={plan} />
        </div>
        {selection && (
          <div
            className="absolute z-10 -translate-x-1/2 -translate-y-full bg-base-100 border border-line shadow-md p-1.5"
            style={{ left: selection.x, top: selection.y }}
          >
            {selection.editing ? (
              <div className="flex items-center gap-1">
                <input
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && draft.trim()) addAnnotation();
                    if (e.key === "Escape") setSelection(null);
                  }}
                  placeholder="comment on selection…"
                  autoFocus
                  className="w-48 bg-transparent outline-none text-xs px-1"
                />
                <button type="button" className="bg-primary text-primary-content px-2.5 py-1 text-xs font-semibold disabled:opacity-50" disabled={!draft.trim()} onClick={addAnnotation}>
                  add
                </button>
              </div>
            ) : (
              <button
                type="button"
                className="bg-primary text-primary-content px-2.5 py-1 text-xs font-semibold"
                onClick={() => setSelection((s) => (s ? { ...s, editing: true } : s))}
              >
                + comment
              </button>
            )}
          </div>
        )}
      </div>
      {annotations.length > 0 && (
        <div className="flex flex-col gap-1.5 mt-3">
          {annotations.map((a, i) => (
            <div key={i} className="flex items-start gap-2 text-xs border border-line bg-base-200/50 px-2.5 py-1.5">
              <div className="flex-1 min-w-0">
                <div className="text-dim2 italic truncate">&quot;{a.quote}&quot;</div>
                <div className="text-text2">{a.comment}</div>
              </div>
              <button
                type="button"
                className="text-dim2 hover:text-error flex-none"
                onClick={() => removeAnnotation(i)}
              >
                ✕
              </button>
            </div>
          ))}
        </div>
      )}
      <div className="flex items-center gap-2 mt-3">
        <ActionButton
          className="bg-primary text-primary-content px-6 py-2 text-base font-semibold disabled:opacity-50"
          busy={busy}
          onClick={onApprove}
        >
          approve
        </ActionButton>
        <button
          type="button"
          className="border border-acc-line px-6 py-2 text-base hover:border-error hover:text-error disabled:opacity-50"
          disabled={busy}
          onClick={() => setFeedbackOpen((v) => !v)}
        >
          request changes
        </button>
      </div>
      {feedbackOpen && (
        <div className="flex items-center gap-2 mt-2">
          <input
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (feedback.trim() || annotations.length > 0)) send();
            }}
            placeholder={annotations.length > 0 ? "add an overall comment (optional)…" : "what should change?"}
            autoFocus
            disabled={busy}
            className="flex-1 min-w-0 bg-transparent border border-line px-3 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2"
          />
          <ActionButton
            className="bg-primary text-primary-content px-4 py-2 text-sm font-semibold disabled:opacity-50 flex-none"
            busy={busy}
            disabled={!feedback.trim() && annotations.length === 0}
            onClick={send}
          >
            send
          </ActionButton>
        </div>
      )}
    </div>
  );
}
