import { useState } from "react";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { ConfirmModal } from "./ConfirmModal";
import { InlineError } from "./InlineError";
import { isPodPhaseLive } from "../pages/SessionList";

// Multi-select over the list, so "archive these twelve" is one gesture rather
// than twelve trips through the actions modal.
//
// Desktop only, deliberately: a checkbox column is chrome a phone has no room
// for, and the phone is the surface for answering one decision away from a
// desk, not for bulk tidying.

// Anchor for shift-click. Held here rather than in the list so the range and
// the selection can't disagree about which click was last.
export function useSelection() {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [anchor, setAnchor] = useState<string | null>(null);

  // `ordered` is the ids as rendered, which is what a shift-range means to the
  // person dragging their eye down the list — not creation order, not id order.
  function toggle(id: string, shiftKey: boolean, ordered: string[]) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (shiftKey && anchor !== null) {
        const from = ordered.indexOf(anchor);
        const to = ordered.indexOf(id);
        if (from !== -1 && to !== -1) {
          const [lo, hi] = from < to ? [from, to] : [to, from];
          // Range always ADDS. A shift-drag that toggles each row flips the
          // ones already picked back off, which reads as the selection
          // randomly losing rows.
          for (let i = lo; i <= hi; i++) next.add(ordered[i]);
          return next;
        }
      }
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
    setAnchor(id);
  }

  function clear() {
    setSelected(new Set());
    setAnchor(null);
  }

  return { selected, toggle, clear };
}

export function SelectBox({
  checked,
  onToggle,
}: {
  checked: boolean;
  onToggle: (shiftKey: boolean) => void;
}) {
  return (
    <input
      type="checkbox"
      checked={checked}
      aria-label="select session"
      // The row underneath is itself clickable — without this, ticking a box
      // also opens the session.
      onClick={(e) => e.stopPropagation()}
      onChange={(e) => onToggle((e.nativeEvent as MouseEvent).shiftKey)}
      className="checkbox checkbox-xs flex-none"
    />
  );
}

type Pending = { kind: "archive" | "kill" | "delete"; label: string } | null;

export function BatchBar({
  sessions,
  onClear,
  reload,
}: {
  // The selected sessions, in render order.
  sessions: Session[];
  onClear: () => void;
  reload: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<string | null>(null);
  const [pending, setPending] = useState<Pending>(null);

  if (sessions.length === 0) return null;

  const live = sessions.filter((s) => isPodPhaseLive(s.podPhase));
  const archivable = sessions.filter((s) => s.archivedAt === undefined);

  // allSettled, not all: one failure must not abandon the other eleven, and
  // the count of each is the whole report. There is no batch RPC and none is
  // needed — the fleet is capped at a handful of live sessions.
  async function runBatch(targets: Session[], verb: string, call: (s: Session) => Promise<unknown>) {
    setBusy(true);
    setResult(null);
    const outcomes = await Promise.allSettled(targets.map(call));
    const failed = outcomes.filter((o) => o.status === "rejected").length;
    setBusy(false);
    reload();
    if (failed === 0) {
      setResult(null);
      onClear();
      return;
    }
    // Partial success is the case worth being loud about: the list reloads and
    // looks plausible either way, so silence here reads as "it all worked".
    setResult(`${targets.length - failed} of ${targets.length} ${verb}, ${failed} failed`);
  }

  const btn = "border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary disabled:opacity-40";

  return (
    <>
      <div className="flex items-center gap-2 flex-wrap border border-primary/40 bg-base-200 px-3 py-2">
        <span className="text-sm text-base-content flex-none">{sessions.length} selected</span>
        <button
          type="button"
          disabled={busy || archivable.length === 0}
          className={btn}
          title={archivable.length === 0 ? "all already archived" : undefined}
          onClick={() => setPending({ kind: "archive", label: `Archive ${archivable.length} session(s)?` })}
        >
          archive {archivable.length > 0 && archivable.length !== sessions.length ? archivable.length : ""}
        </button>
        <button
          type="button"
          disabled={busy || live.length === 0}
          className={btn}
          title={live.length === 0 ? "none have a live pod" : undefined}
          onClick={() => setPending({ kind: "kill", label: `Kill ${live.length} running session(s)?` })}
        >
          kill {live.length > 0 && live.length !== sessions.length ? live.length : ""}
        </button>
        <button
          type="button"
          disabled={busy}
          className="border border-pink-line text-error px-3 py-1 text-sm hover:bg-pink-chip disabled:opacity-40"
          onClick={() => setPending({ kind: "delete", label: `Delete ${sessions.length} session(s)?` })}
        >
          delete
        </button>
        <button type="button" onClick={onClear} className="ml-auto text-xs text-dim2 hover:text-base-content flex-none">
          clear ✕
        </button>
      </div>
      {result && <InlineError message={result} onDismiss={() => setResult(null)} />}

      {/* Delete destroys the transcript, so past one session it asks for the
          word — the same gate ManageReposModal uses. Archive is reversible and
          Kill only ends a pod, so those get a plain confirm. */}
      <ConfirmModal
        open={pending !== null}
        title={pending?.label ?? ""}
        message={
          pending?.kind === "delete"
            ? "This force-tears-down any live pod and deletes the session, including its transcript. There is no undo."
            : pending?.kind === "kill"
              ? "Ends the pod for each running session. The session itself stays and can be warmed again."
              : "Marks each session finished. Reversible — an archived session can still be warmed."
        }
        confirmWord={pending?.kind === "delete" && sessions.length > 1 ? "delete" : undefined}
        confirmLabel={pending?.kind ?? "confirm"}
        danger={pending?.kind !== "archive"}
        onCancel={() => setPending(null)}
        onConfirm={() => {
          const p = pending;
          setPending(null);
          if (!p) return;
          if (p.kind === "archive")
            void runBatch(archivable, "archived", (s) => client.archiveSession({ sessionId: s.id }));
          else if (p.kind === "kill") void runBatch(live, "killed", (s) => client.stopSession({ sessionId: s.id }));
          else void runBatch(sessions, "deleted", (s) => client.deleteSession({ sessionId: s.id }));
        }}
      />
    </>
  );
}
