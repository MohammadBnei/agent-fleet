import { useEffect, useState } from "react";
import { Modal } from "./Modal";

// Typed-confirmation gate for the one permission mode that removes the
// canUseTool Write/Edit gate entirely (docs/adr/0027) — a text input the
// human must match before the confirm button un-disables, so a stray click
// can't fire it the way a plain button could.
export function BypassConfirmModal({
  open,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const [typed, setTyped] = useState("");

  useEffect(() => {
    setTyped("");
  }, [open]);

  const confirmEnabled = typed.trim() === "bypass";

  return (
    <Modal open={open} onClose={onCancel}>
      <h3 className="font-semibold text-base text-warning mb-2">Bypass permissions?</h3>
      <p className="text-sm">
        This disables the write/edit approval gate for this task for the rest of the session — every tool call,
        including Write and Edit, will run without asking again.
      </p>
      <p className="text-sm mt-3">
        Type <code className="font-mono">bypass</code> to confirm.
      </p>
      <input
        type="text"
        value={typed}
        onChange={(e) => setTyped(e.target.value)}
        className="input input-sm input-bordered w-full mt-2"
        placeholder="bypass"
        autoFocus
      />
      <div className="modal-action">
        <button type="button" className="btn btn-sm" onClick={onCancel}>
          Cancel
        </button>
        <button type="button" className="btn btn-sm btn-error" disabled={!confirmEnabled} onClick={onConfirm}>
          Bypass permissions
        </button>
      </div>
    </Modal>
  );
}
