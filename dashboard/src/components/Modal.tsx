import { useEffect, useRef, type ReactNode } from "react";

type ModalProps = {
  open: boolean;
  onClose: () => void;
  children: ReactNode;
  boxClassName?: string;
};

// The one modal shell for the whole dashboard — daisyUI's responsive
// pattern (bottom sheet on mobile, centered box on desktop) instead of each
// dialog picking its own. Native <dialog>.showModal()/.close() driven by
// `open` gets focus-trap/ESC/backdrop-click dismissal for free.
export function Modal({ open, onClose, children, boxClassName = "" }: ModalProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    if (open) dialogRef.current?.showModal();
    else dialogRef.current?.close();
  }, [open]);

  return (
    <dialog
      ref={dialogRef}
      className="modal modal-bottom sm:modal-middle"
      // Only react to THIS dialog closing. The native `close` event doesn't
      // bubble, but React's synthetic system replays it up the tree anyway —
      // so a Modal rendered inside another open Modal closed both at once,
      // and dismissing an inner drawer dropped the human two levels out.
      // Confirmed in Playwright on the mobile panels sheet, whose bottom
      // sheet contains the E2E Manage drawer.
      //
      // ManageReposModal worked around this by rendering its confirm dialog
      // as a sibling of the modal that triggers it; with the guard here that
      // workaround is no longer required of every caller.
      onClose={(e) => {
        if (e.target !== dialogRef.current) return;
        onClose();
      }}
    >
      <div className={`modal-box ${boxClassName}`}>{children}</div>
      <form method="dialog" className="modal-backdrop">
        <button>close</button>
      </form>
    </dialog>
  );
}
