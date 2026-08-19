import type { Session } from "../gen/agentfleet/v1/core_pb";
import type { ListSummary } from "../transcript";
import { Modal } from "./Modal";
import { DecisionInline } from "./DecisionInline";
import { sessionLabel } from "../sessionLabel";
import { blockedForLabel } from "../pages/SessionList";

// Every pending decision, answerable without leaving where you are.
//
// The header badge used to navigate: it set the sessions view and turned on the
// needs-you filter. From inside a session detail that did nothing visible at
// all (the view was already "sessions", so selectView kept the open session),
// and from anywhere else it threw away whatever you were reading to get to a
// list you then had to click into. A decision is a 5-second interruption; it
// should not cost your place.
//
// No new RPC and no new fetch: App already polls `sessions` and already holds a
// ListSummary per active session for the list's inline decisions. This is the
// same DecisionInline the list row renders, stacked.
export function NeedsYouModal({
  open,
  sessions,
  summaries,
  onClose,
  onOpenSession,
  reload,
}: {
  open: boolean;
  // Already filtered to the blocked ones by the caller, on the SAME predicate
  // the badge counts — if the two ever diverge the badge says 3 and this
  // renders empty, which reads as the button being broken.
  sessions: Session[];
  summaries: Map<string, ListSummary>;
  onClose: () => void;
  onOpenSession: (id: string) => void;
  reload: () => void;
}) {
  // Nothing at all while closed. Every other Modal in the app renders its
  // children regardless, which is harmless for a form — but this one mirrors
  // the whole blocked list, so a closed dialog kept a second live DecisionInline
  // per blocked session in the DOM, each with its own state and its own
  // allow/deny buttons, re-rendering on every 5s poll. Caught by Playwright
  // resolving two elements for one session label with the dialog shut.
  if (!open) return null;

  return (
    <Modal open={open} onClose={onClose} boxClassName="max-w-3xl max-h-[85vh] overflow-hidden flex flex-col">
      <div className="flex items-baseline gap-2 mb-3 flex-none">
        <span className="text-2xs tracking-[0.12em] text-error">WAITING ON YOU</span>
        <span className="flex-1 h-px bg-pink-line" />
        <span className="text-xs text-dim2">{sessions.length}</span>
      </div>
      {/* min-h-0 or the column refuses to scroll and the sheet grows instead. */}
      <div className="flex flex-col gap-4 overflow-y-auto min-h-0">
        {sessions.length === 0 ? (
          // Reachable for real: a decision can resolve between the 5s poll that
          // drew the badge and the click on it.
          <div className="text-sm text-dim2">Nothing waiting any more.</div>
        ) : (
          sessions.map((s) => {
            const summary = summaries.get(s.id);
            const blockedFor = blockedForLabel(s);
            const queued = summary?.pendingPermissionCount ?? 0;
            return (
              <div key={s.id} className="border border-pink-line">
                {/* Title first, id demoted to the meta line — same ordering as
                    the list rows and the actions modal, so the thing you scan
                    for is always in the same place. */}
                <div className="flex items-baseline gap-2 px-3.5 pt-3 min-w-0">
                  <button
                    type="button"
                    onClick={() => onOpenSession(s.id)}
                    className="text-sm font-semibold min-w-0 truncate text-left hover:text-primary cursor-pointer flex-1"
                  >
                    {sessionLabel(s)}
                  </button>
                  <span className="text-xs text-dim2 flex-none">
                    {s.repo}
                    {blockedFor ? ` · ${blockedFor}` : ""}
                    {queued > 1 ? ` · ${queued} decisions` : ""}
                  </span>
                </div>
                <div className="px-3.5 pt-0.5 text-2xs text-dim2">#{s.id.slice(0, 8)}</div>
                {/* Same component the list row uses, so a plan still refuses to
                    be approved from a summary line and sends you into the
                    session instead (DecisionInline's own reasoning). */}
                <DecisionInline
                  session={s}
                  summary={summary}
                  reload={reload}
                />
              </div>
            );
          })
        )}
      </div>
    </Modal>
  );
}
