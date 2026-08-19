import { Modal } from "./Modal";
import { Markdown } from "./Markdown";
import { ActionButton } from "./ActionButton";

// A plan, at full length, with its decision attached.
//
// The list could only ever show a plan badly: a scroll box a few lines tall
// inside a row, competing with every other session on the page. The escape was
// "read it first", which navigated away — so reading the thing cost you your
// place in the list, and the decision then had to be made from a different
// screen than the one you read it on.
//
// This is the reading surface: the full page, a real measure, and the same
// three answers underneath, so judging and deciding happen in one place.
//
// Full-bleed rather than a centred box. A plan is the longest thing the console
// ever asks anyone to read, and a dialog floating over the list kept the thing
// you were reading and the thing you were ignoring on screen at once. At this
// size the page IS the plan.
//
// Full width, not a centred measure: a plan is not only prose. It carries
// diagrams, tables and code, and those were the things a 760px column squeezed
// while the page had room either side of them. The fleet's agents are told to
// open a plan with a mermaid diagram, so the wide things are the point.
//
// rounded-none and a full viewport: the modal-box radius and margin are what
// make a dialog read as a floating card rather than a page. dvh, not vh, or
// mobile browser chrome crops the actions off the bottom.
export function PlanModal({
  open,
  plan,
  busy,
  pending,
  onApproveAuto,
  onApprove,
  onRequestChanges,
  onClose,
}: {
  open: boolean;
  plan: string;
  busy: boolean;
  pending: string | null;
  onApproveAuto: () => void;
  onApprove: () => void;
  onRequestChanges: () => void;
  onClose: () => void;
}) {
  return (
    <Modal open={open} onClose={onClose} boxClassName="max-w-none w-screen h-[100dvh] max-h-none rounded-none p-5 sm:p-8 overflow-hidden flex flex-col">
      <div className="flex items-baseline gap-2 mb-4 flex-none w-full">
        <span className="text-2xs tracking-[0.12em] text-error">PLAN — NEEDS YOUR REVIEW</span>
        <span className="flex-1 h-px bg-pink-line" />
      </div>
      {/* min-h-0 or the column refuses to scroll and the dialog grows instead.
          The prose is capped at the feed's measure and centred: this is the one
          place in the console whose entire job is reading, and a plan set at the
          full width of a 4xl dialog is the same wall it was in the row. */}
      <div className="overflow-y-auto min-h-0 flex-1">
        <div className="text-base leading-[1.7]">
          <Markdown text={plan} />
        </div>
      </div>
      {/* Pinned below the scroll area, not after it: the decision must not be
          something you have to reach the bottom of a long plan to find. */}
      <div className="flex-none flex flex-wrap items-center gap-2.5 pt-4 mt-4 border-t border-line w-full">
        <ActionButton
          busy={pending === "allow"}
          disabled={busy}
          className="bg-primary text-primary-content font-semibold px-5 py-2 text-sm cursor-pointer disabled:opacity-50"
          onClick={onApproveAuto}
        >
          approve + auto
        </ActionButton>
        <button
          type="button"
          disabled={busy}
          onClick={onApprove}
          className="border border-acc-line px-5 py-2 text-sm cursor-pointer hover:border-primary disabled:opacity-50"
        >
          approve only
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={onRequestChanges}
          className="border border-acc-line px-5 py-2 text-sm cursor-pointer hover:border-primary disabled:opacity-50"
        >
          request changes
        </button>
        <button type="button" onClick={onClose} className="ml-auto text-xs text-dim2 hover:text-base-content cursor-pointer">
          close
        </button>
      </div>
    </Modal>
  );
}
