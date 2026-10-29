import { useState } from "react";
import { client } from "../connectClient";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { sessionLabel } from "../sessionLabel";
import { shortId } from "../sessionState";
import { useBusyAction } from "../useBusyAction";
import { ActionsMenu } from "./ActionsMenu";
import { AutoModeConfirm } from "./AutoModeConfirm";
import { InlineError } from "./InlineError";
import { Modal } from "./Modal";

// Interrupt / Stop / Warm / Archive / Mode, from a list row.
//
// ActionsMenu already did all of this and derives Warm-vs-Stop from podPhase
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
  // that succeeds ends in a reload — and closes the menu: it used to stay up
  // showing the same buttons after a Stop, so nothing said the click had
  // landed. Failures leave both alone and surface inline — reloading on
  // failure would repaint the row as if nothing happened.
  const run = (action: () => Promise<unknown>, key: string) => {
    void runAction(action, key).then((ok) => {
      if (!ok) return;
      reload();
      onClose();
    });
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
              <span className="text-sm font-semibold break-words">
                {sessionLabel(session)}
              </span>
              <span className="text-2xs text-dim2 break-all">
                {session.repo} · #{shortId(session.id)}
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
              liveState={session.liveState}
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
      <AutoModeConfirm
        open={autoOpen}
        onCancel={() => setAutoOpen(false)}
        onConfirm={() => {
          setAutoOpen(false);
          if (session)
            run(
              () =>
                client.setPermissionMode({
                  sessionId: session.id,
                  mode: "auto",
                }),
              "action:mode",
            );
        }}
      />
    </>
  );
}
