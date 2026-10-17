import { SelectBox } from "./BatchActions";
import { ActionButton } from "./ActionButton";
import { useBusyAction } from "../useBusyAction";
import { client } from "../connectClient";
import type { Session } from "../gen/agentfleet/v1/core_pb";

// A row's own one-click action (interrupt / warm / archive). These were six
// `void client.x().then(reload)` calls: no spinner, and a rejection went
// nowhere, so a warm that failed looked exactly like a warm still booting.
// Now the button spins while the call is out, and a failure stays on the
// button itself ("warm failed", the reason in its tooltip) — a row has no room
// for an error box, and this keeps the failure next to the thing that failed.
export function RowAction({
  label,
  action,
  reload,
  className,
}: {
  label: string;
  action: () => Promise<unknown>;
  reload: () => void;
  className: string;
}) {
  const { busyKey, error, run } = useBusyAction();
  return (
    <ActionButton
      busy={busyKey !== null}
      title={error ?? undefined}
      className={`${className} ${error ? "border-error! text-error" : ""}`}
      onClick={() => void run(action, label).then(reload)}
    >
      {error ? `${label} failed` : label}
    </ActionButton>
  );
}

// A STUCK row's actions, on both form factors: interrupt a live turn, or warm
// a session whose pod is gone — never a swept one, whose working directory the
// retention GC took, so a warm would resume nothing — plus its log. The two
// lists each wrote this out; only the button size differs.
export function StuckActions({
  session,
  live,
  reload,
  onOpenLogs,
  buttonClass,
}: {
  session: Session;
  live: boolean;
  reload: () => void;
  onOpenLogs: () => void;
  buttonClass: string;
}) {
  return (
    <>
      {live ? (
        <RowAction
          label="interrupt"
          action={() => client.interrupt({ sessionId: session.id })}
          reload={reload}
          className={buttonClass}
        />
      ) : (
        session.sweptAt === undefined && (
          <RowAction
            label="warm"
            action={() => client.warmSession({ sessionId: session.id })}
            reload={reload}
            className={buttonClass}
          />
        )
      )}
      <button type="button" onClick={onOpenLogs} className={buttonClass}>
        read log
      </button>
    </>
  );
}

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
