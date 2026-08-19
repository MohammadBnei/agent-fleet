import { useEffect, useState } from "react";
import { Modal } from "./Modal";

// A dialog for the answer that did not fit a button: the free-text response to
// a question, and the change request on a plan. Both wanted the same thing — an
// input, send, cancel — and the second one is why this is a component rather
// than a copy.
//
// The draft is local and seeded from `initial` each time the dialog opens, so
// cancel discards the edit without touching whatever the caller had already
// saved. Sharing one variable between the two is how "cancel" ends up blanking
// the answer you were editing.
export function TextResponseModal({
  open,
  title,
  context,
  placeholder,
  initial = "",
  submitLabel = "send",
  busy,
  onSubmit,
  onClose,
}: {
  open: boolean;
  title: string;
  // The thing being responded to, restated — the dialog covers it, and
  // answering something you can no longer see is guesswork.
  context?: string;
  placeholder: string;
  initial?: string;
  submitLabel?: string;
  busy?: boolean;
  onSubmit: (text: string) => void;
  onClose: () => void;
}) {
  const [draft, setDraft] = useState(initial);

  // Re-seed on each open. Without this the dialog keeps the previous draft
  // when reopened for a different question.
  useEffect(() => {
    if (open) setDraft(initial);
  }, [open, initial]);

  const submit = () => {
    if (!draft.trim()) return;
    onSubmit(draft);
    onClose();
  };

  return (
    <Modal open={open} onClose={onClose} boxClassName="max-w-xl">
      <div className="flex flex-col gap-3">
        <span className="text-2xs tracking-[0.12em] text-dim2">{title}</span>
        {context && <span className="text-sm text-dim break-words">{context}</span>}
        <textarea
          autoFocus
          rows={5}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            // Cmd/Ctrl+Enter sends. A bare Enter stays a newline — this is the
            // field for the response that needed more than one line.
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit();
          }}
          placeholder={placeholder}
          className="w-full border border-acc-line bg-base-300/40 px-2.5 py-2 text-sm resize-y focus:border-primary focus:outline-none"
        />
        <div className="flex items-center gap-2.5">
          <button
            type="button"
            onClick={submit}
            disabled={busy || !draft.trim()}
            className="bg-primary text-primary-content font-semibold px-4 py-1.5 text-sm cursor-pointer disabled:opacity-40"
          >
            {submitLabel}
          </button>
          <button
            type="button"
            onClick={onClose}
            className="border border-acc-line px-4 py-1.5 text-sm cursor-pointer hover:border-primary"
          >
            cancel
          </button>
          <span className="text-2xs text-dim2 ml-auto">⌘⏎ to {submitLabel}</span>
        </div>
      </div>
    </Modal>
  );
}
