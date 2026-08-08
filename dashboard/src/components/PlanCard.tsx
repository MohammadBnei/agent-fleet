import { useRef, useState } from "react";
import { Markdown } from "./Markdown";

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
  onApprove,
  onFeedback,
  edgeClassName,
  allowAnnotate = true,
}: {
  plan: string;
  pending: boolean;
  busy: boolean;
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
  const wrapperRef = useRef<HTMLDivElement>(null);

  if (!pending) {
    return (
      <div className="flex items-center gap-1.5 text-[10.5px] text-base-content/40">
        <span className="badge badge-ghost badge-xs">plan</span>
        <span className="truncate">{plan.split("\n")[0]}</span>
      </div>
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
    <div className={`border-y-2 border-primary/40 bg-primary/[0.04] py-4 ${edgeClassName}`}>
      <div className="text-[10px] tracking-[0.12em] font-semibold text-primary mb-2">PLAN — NEEDS YOUR REVIEW</div>
      <div ref={wrapperRef} className="relative">
        <div
          onMouseUp={handleMouseUp}
          className="text-[12px] leading-relaxed text-base-content/90 max-h-[50vh] overflow-y-auto"
        >
          <Markdown text={plan} />
        </div>
        {selection && (
          <div
            className="absolute z-10 -translate-x-1/2 -translate-y-full bg-base-100 border border-base-content/15 rounded-lg shadow-md p-1.5"
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
                  className="w-48 bg-transparent outline-none text-[11px] px-1"
                />
                <button type="button" className="btn btn-primary btn-xs" disabled={!draft.trim()} onClick={addAnnotation}>
                  Add
                </button>
              </div>
            ) : (
              <button
                type="button"
                className="btn btn-primary btn-xs"
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
            <div key={i} className="flex items-start gap-2 text-[11px] bg-base-200/50 rounded-md px-2.5 py-1.5">
              <div className="flex-1 min-w-0">
                <div className="text-base-content/50 italic truncate">&quot;{a.quote}&quot;</div>
                <div className="text-base-content/90">{a.comment}</div>
              </div>
              <button
                type="button"
                className="text-base-content/40 hover:text-base-content/70 flex-none"
                onClick={() => removeAnnotation(i)}
              >
                ✕
              </button>
            </div>
          ))}
        </div>
      )}
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
              if (e.key === "Enter" && (feedback.trim() || annotations.length > 0)) send();
            }}
            placeholder={annotations.length > 0 ? "add an overall comment (optional)…" : "what should change?"}
            autoFocus
            disabled={busy}
            className="flex-1 bg-transparent border border-base-content/15 rounded-lg px-3 py-2 text-[12px] outline-none focus:border-primary/50"
          />
          <button
            type="button"
            className="btn btn-primary btn-sm"
            disabled={busy || (!feedback.trim() && annotations.length === 0)}
            onClick={send}
          >
            Send
          </button>
        </div>
      )}
    </div>
  );
}
