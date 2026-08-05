import { useEffect, useRef } from "react";

// Shared error surface — daisyUI dialog/modal, same pattern NewTaskDialog
// already established, instead of a persistent inline `alert alert-error`
// banner. Auto-opens when `message` goes non-null, calls onClose (clears
// the parent's error state) on any dismissal so a stale message can't
// silently reopen the dialog on the next render.
export function ErrorModal({ message, onClose }: { message: string | null; onClose: () => void }) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    if (message) dialogRef.current?.showModal();
    else dialogRef.current?.close();
  }, [message]);

  return (
    <dialog ref={dialogRef} className="modal" onClose={onClose}>
      <div className="modal-box">
        <h3 className="font-semibold text-base text-error mb-2">Something went wrong</h3>
        <p className="text-sm whitespace-pre-wrap">{message}</p>
        <div className="modal-action">
          <button type="button" className="btn btn-sm" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
      <form method="dialog" className="modal-backdrop">
        <button>close</button>
      </form>
    </dialog>
  );
}
