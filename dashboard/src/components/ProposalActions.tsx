import { client } from "../connectClient";

// The approve/dismiss bar for a machine-created proposal — an Alertmanager
// alert or a due scheduled audit, neither of which had a human in the loop
// and both of which run an agent with cluster access (docs/adr/0037).
//
// Deliberately NOT inside ActionsMenu: that lives behind a ⚙ modal on both
// platforms, and burying the only meaningful action for a proposal behind
// two clicks is the wrong affordance for a screen whose entire purpose is
// a yes/no. Shared by desktop and mobile so the two can't drift — mobile's
// task detail otherwise duplicates nearly all of desktop's.
//
// Dismiss is plain deleteTask: its soft delete drops the row out of the
// alert dedup index, so a still-firing alert is proposed again on its next
// fire. "Not now", not "never" — silencing an alert for good is
// Alertmanager's job, and a fleet that quietly stopped surfacing a firing
// alert would be discovered during an incident.
export function ProposalActions({
  taskId,
  busy,
  run,
  onDismissed,
}: {
  taskId: string;
  busy: boolean;
  run: (action: () => Promise<unknown>, key: string) => void;
  onDismissed?: () => void;
}) {
  return (
    <div className="alert alert-warning flex flex-wrap items-center gap-3 py-2">
      <span className="text-[13px]">
        <span className="font-semibold">Proposal</span> — not dispatched yet. Nothing runs until you approve it.
      </span>
      <div className="flex items-center gap-2 ml-auto">
        <button
          type="button"
          className="btn btn-success btn-xs"
          disabled={busy}
          onClick={() => run(() => client.approveTask({ taskId }), "proposal")}
        >
          Approve &amp; dispatch
        </button>
        <button
          type="button"
          className="btn btn-ghost btn-xs"
          disabled={busy}
          onClick={() =>
            run(async () => {
              await client.deleteTask({ taskId });
              onDismissed?.();
            }, "proposal")
          }
        >
          Dismiss
        </button>
      </div>
    </div>
  );
}
