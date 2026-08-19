import { SelectBox } from "./BatchActions";

// The two controls every session row carries. Both were written twice — once
// as an absolutely-positioned overlay for the tall cards, once inline for the
// one-line rows — which is how the phone ended up with a 20px tap target and
// the desktop with a checkbox in a different place per row kind.
//
// `inline` picks the placement; everything else is shared. Hidden at rest and
// revealed on hover or once checked, so a row at rest opens on its title
// rather than on a bulk-action control.
const REVEAL =
  "opacity-0 group-hover:opacity-100 focus-within:opacity-100 has-[:checked]:opacity-100 transition-opacity";

export function RowSelect({
  picked,
  onPick,
  inline,
}: {
  picked: boolean;
  onPick: (shiftKey: boolean) => void;
  inline?: boolean;
}) {
  return (
    <div className={inline ? `flex-none ${REVEAL}` : `absolute top-1.5 right-[50px] flex items-center ${REVEAL}`}>
      <SelectBox checked={picked} onToggle={onPick} />
    </div>
  );
}

export function RowActionsButton({ onOpen, inline }: { onOpen: () => void; inline?: boolean }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      title="Session actions"
      aria-label="Session actions"
      className={
        inline
          ? // Phone: a real target, not a 20px corner overlay parked against
            // the card edge — that is the tap this form factor gets wrong.
            "flex-none -my-1 px-2 py-1 text-dim2 hover:text-primary text-sm cursor-pointer"
          : "absolute top-1.5 right-7 w-5 h-5 flex items-center justify-center text-dim2 hover:text-primary text-xs cursor-pointer"
      }
    >
      ⋯
    </button>
  );
}
