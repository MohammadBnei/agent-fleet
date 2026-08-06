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
    <dialog ref={dialogRef} className="modal modal-bottom sm:modal-middle" onClose={onClose}>
      <div className={`modal-box ${boxClassName}`}>{children}</div>
      <form method="dialog" className="modal-backdrop">
        <button>close</button>
      </form>
    </dialog>
  );
}
