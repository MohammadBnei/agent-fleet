import { useEffect, useState } from "react";
import { Modal } from "./Modal";

// Generic confirmation modal — replaces window.confirm everywhere in the
// dashboard (native dialogs can't be themed/tested, block the render
// thread, and a stray click can fire them same as a plain button). Plain
// Cancel/Confirm by default; passing confirmWord upgrades it to a typed
// gate (mirrors BypassConfirmModal's require-typing pattern) for the rarer,
// more consequential actions — confirmWord's presence is what drives the
// stricter UX, not a blanket rule. Callers hold their own "what am I
// confirming" state and pass open/onConfirm/onCancel — see App.tsx's
// deleteSession, ManageReposModal.tsx's
// pendingDelete (typed) for both patterns.
export function ConfirmModal({
  open,
  title = "Are you sure?",
  message,
  confirmWord,
  confirmLabel = "Delete",
  danger = true,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title?: string;
  message: string;
  confirmWord?: string;
  confirmLabel?: string;
  danger?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const [typed, setTyped] = useState("");

  useEffect(() => {
    setTyped("");
  }, [open]);

  const confirmEnabled = !confirmWord || typed.trim() === confirmWord;

  return (
    <Modal open={open} onClose={onCancel}>
      <h3 className="font-semibold text-base mb-2">{title}</h3>
      <p className="text-sm text-text2">{message}</p>
      {confirmWord && (
        <>
          <p className="text-sm mt-3">
            Type <code className="font-mono">{confirmWord}</code> to confirm.
          </p>
          <input
            type="text"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            className="input input-sm input-bordered w-full mt-2"
            placeholder={confirmWord}
            autoFocus
          />
        </>
      )}
      <div className="modal-action">
        <button type="button" className="btn btn-sm" onClick={onCancel}>
          Cancel
        </button>
        <button
          type="button"
          className={`btn btn-sm ${danger ? "btn-error" : "btn-primary"}`}
          disabled={!confirmEnabled}
          onClick={onConfirm}
        >
          {confirmLabel}
        </button>
      </div>
    </Modal>
  );
}
