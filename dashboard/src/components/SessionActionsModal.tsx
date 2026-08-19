import { useState } from "react";
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
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [autoOpen, setAutoOpen] = useState(false);

  // Same contract as useSessionDetail's run(): key the spinner to the button
  // that was clicked, surface the failure inline, always clear busy. The list
  // has no stream to reflect the change back, so every action ends in reload().
  function run(action: () => Promise<unknown>, key: string) {
    setBusyKey(key);
    setError(null);
    action()
      .then(() => reload())
      .catch((err: Error) => setError(err.message))
      .finally(() => setBusyKey(null));
  }

  return (
    <>
      <Modal
        open={session !== null}
        onClose={() => {
          setError(null);
          onClose();
        }}
        boxClassName="max-w-md"
      >
        {session && (
          <div className="flex flex-col gap-3">
            <div className="flex items-baseline gap-2 min-w-0">
              <span className="text-sm font-semibold flex-none">#{session.id.slice(0, 6)}</span>
              <span className="text-sm text-dim min-w-0 truncate">{sessionLabel(session)}</span>
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
