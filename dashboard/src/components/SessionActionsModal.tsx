import { useState } from "react";
import { useBusyAction } from "../useBusyAction";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import { InlineError } from "./InlineError";
import { ActionsMenu } from "./ActionsMenu";
import { AUTO_MODE_WARNING } from "../approvePlan";
import { sessionLabel } from "../sessionLabel";

// Interrupt / Kill / Warm / Archive / Mode, from a list row.
//
// ActionsMenu already did all of this and derives Warm-vs-Kill from podPhase
// itself — it was just mounted only from SessionPanels, i.e. only once you had
// opened the session. Nothing new is built here: this is the `run`/`busyKey`
// pair from useSessionDetail (the part ActionsMenu needs and a list does not
// otherwise have) plus the auto-mode confirm the detail view wraps it in.
//
// Mounted ONCE per list, keyed by which session is open, rather than one per
// row — 40 rows meant 40 dialogs and 40 pieces of busy state.
export function SessionActionsModal({
  session,
  onClose,
  onDelete,
  reload,
}: {
  // null = closed.
  session: Session | null;
  onClose: () => void;
  // Routed up to App's own ConfirmModal rather than confirmed here — deleting
  // destroys the transcript, and App already owns that gate for the ✕ overlay.
  // The taller cards have that ✕; CompactRow does not, so for a quiet session
  // this menu is the ONLY way to delete one.
  onDelete: (id: string) => void;
  reload: () => void;
}) {
  const [autoOpen, setAutoOpen] = useState(false);
  const { busyKey, error, run: runAction, clearError } = useBusyAction();

  // The list has no transcript stream to reflect a change back, so every action
  // that succeeds ends in a reload. Failures leave the list alone and surface
  // inline — reloading on failure would repaint the row as if nothing happened.
  const run = (action: () => Promise<unknown>, key: string) => {
    void runAction(action, key).then((ok) => ok && reload());
  };

  return (
    <>
      <Modal
        open={session !== null}
        onClose={() => {
          clearError();
          onClose();
        }}
        boxClassName="max-w-md"
      >
        {session && (
          <div className="flex flex-col gap-3">
            {/* The list rows dropped the `#abc123` prefix — six hex characters
                at the front of every row, unreadable and unsearchable, pushing
                the title (the only part anyone scans for) to the right. This is
                where it lives now, with room to be labelled and to show more of
                itself. */}
            <div className="flex flex-col gap-1 min-w-0">
              <span className="text-sm font-semibold break-words">{sessionLabel(session)}</span>
              <span className="text-2xs text-dim2 break-all">
                {session.repo} · #{session.id.slice(0, 8)}
              </span>
            </div>
            {error && <InlineError message={error} />}
            <ActionsMenu
              sessionId={session.id}
              busy={busyKey !== null}
              busyKey={busyKey}
              run={run}
              sweptAt={session.sweptAt}
              archivedAt={session.archivedAt}
              currentMode={session.permissionMode}
              podPhase={session.podPhase}
              onAutoClick={() => setAutoOpen(true)}
            />
            <button
              type="button"
              disabled={busyKey !== null}
              onClick={() => {
                // Close first: App's ConfirmModal is a separate <dialog>, and
                // stacking it on top of this one is the nesting Modal.tsx:36
                // guards against. Closing also means the confirm is not sitting
                // over a menu whose session it is about to delete.
                const id = session.id;
                onClose();
                onDelete(id);
              }}
              className="self-start border border-pink-line text-error px-3 py-1 text-xs hover:bg-pink-chip disabled:opacity-40 cursor-pointer"
            >
              Delete session
            </button>
          </div>
        )}
      </Modal>
      {/* A sibling of the Modal, not a child. `auto` is the one mode that grants
          real authority (everything but rm/sudo without asking), so it goes
          through a confirm rather than firing off the menu — the same routing
          SessionDetail gives it. Rendered outside so the nested-<dialog> close
          replay never applies, which is the trap Modal.tsx:36 documents. */}
      <ConfirmModal
        title="Switch to auto mode?"
        message={AUTO_MODE_WARNING}
        confirmLabel="switch to auto"
        danger={false}
        open={autoOpen}
        onCancel={() => setAutoOpen(false)}
        onConfirm={() => {
          setAutoOpen(false);
          if (session) run(() => client.setPermissionMode({ sessionId: session.id, mode: "auto" }), "action:mode");
        }}
      />
    </>
  );
}

// The `⋯` that opens it. Sits beside DeleteButton in the same absolute corner,
// so a row's two overlays do not each pick their own offset.
export function RowActionsButton({ onOpen }: { onOpen: () => void }) {
  return (
    <button
      type="button"
      onClick={onOpen}
      title="Session actions"
      aria-label="Session actions"
      className="absolute top-1.5 right-7 w-5 h-5 flex items-center justify-center text-dim2 hover:text-primary text-xs"
    >
      ⋯
    </button>
  );
}
