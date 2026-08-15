import type { CSSProperties } from "react";
import { client } from "../connectClient";
import { ActionButton } from "./ActionButton";
import { isPodPhaseLive } from "../pages/TaskList";

// The user-facing SDK modes worth a direct button — "delegate"/"dontAsk"
// are SDK-internal/secondary, never surfaced here. "bypassPermissions" is
// deliberately routed through onBypassClick's confirm modal, not called
// directly like the other three, since it disables the canUseTool gate
// outright for the rest of the session.
const MODES = [
  { value: "default", label: "Default" },
  { value: "plan", label: "Plan" },
  { value: "acceptEdits", label: "Accept edits" },
] as const;

// The Kill/Interrupt/Kill-e2e/Mode/Open-code-server button row, shared
// between desktop and mobile — both render it inside a Modal, opened from a
// small icon button rather than either platform picking its own layout.
// Approve is gone as of the sessions redesign (supersedes docs/adr/0021/
// 0025's phase-boundary framing) — there's no plan->default flip left to
// fix a button to; the Mode dropdown below is the only lever now, and
// highlights whichever mode is actually active instead of just offering to
// change it.
//
// Kill (was "Stop") ends the whole session/pod — a cooperative abort with a
// grace-period force-teardown backstop, per DashboardService.Kill.
// Interrupt is the lighter sibling: stops only the current turn via the
// SDK's own q.interrupt(), session/pod stay alive, per
// DashboardService.Interrupt — also covers cancelling a single in-flight
// tool call, since that tool call *is* the current turn.
// hideToolsInFeed/hideChangesInFeed only ever change what mobile's inline
// exchange-zone feed shows (desktop keeps these in dedicated panels
// regardless), but the toggle itself lives here — the one place both
// platforms already have — so both write the same localStorage keys
// instead of each platform getting its own copy of the control. Optional
// only so ActionsMenu doesn't require them if a future caller has no use
// for this toggle.
export function ActionsMenu({
  sessionId,
  busy,
  busyKey,
  run,
  sweptAt,
  archivedAt,
  currentMode,
  podPhase,
  onBypassClick,
  hideToolsInFeed,
  onHideToolsInFeedChange,
  hideChangesInFeed,
  onHideChangesInFeedChange,
}: {
  sessionId: string;
  // Whether ANY action anywhere on the page is in flight, not just this
  // menu's own — these buttons are page-level/singular (Kill especially),
  // so they stay conservative and disable together with everything else
  // rather than getting their own independent key.
  busy: boolean;
  // Which one is in flight, so the spinner lands on the button that was
  // actually clicked instead of every button greying out together — the
  // whole point being to tell the clicker their click registered.
  busyKey: string | null;
  run: (action: () => Promise<unknown>, key: string) => void;
  // Set once the retention GC has reclaimed this session's working directory
  // (docs/adr/0048). Readable history, not resumable — so Warm is hidden
  // rather than shown and broken.
  sweptAt?: string;
  // Set when a human marked this session finished — the only terminal state
  // the fleet has, because it is the only one a machine cannot compute.
  archivedAt?: string;
  // Unset for an idle/never-warmed session — no mode has been explicitly
  // chosen yet (the SDK itself starts a fresh session in "default", but
  // that's not durable here until SetPermissionMode is actually called).
  currentMode?: string;
  // Drives Warm vs. Stop below — the same pod_phase TaskList's own badges
  // already read, just here to answer "is there a pod to talk to right
  // now" instead of "what does it look like in the list."
  podPhase?: string;
  onBypassClick: () => void;
  hideToolsInFeed?: boolean;
  onHideToolsInFeedChange?: (value: boolean) => void;
  hideChangesInFeed?: boolean;
  onHideChangesInFeedChange?: (value: boolean) => void;
}) {
  const live = isPodPhaseLive(podPhase);
  return (
    <div className="flex flex-col gap-3">
      {(onHideToolsInFeedChange || onHideChangesInFeedChange) && (
        <div className="flex items-center gap-3 text-sm text-text2">
          {onHideToolsInFeedChange && (
            <label className="flex items-center gap-1.5 cursor-pointer">
              <input
                type="checkbox"
                checked={!hideToolsInFeed}
                onChange={(e) => onHideToolsInFeedChange(!e.target.checked)}
                className="checkbox checkbox-sm"
              />
              Tools
            </label>
          )}
          {onHideChangesInFeedChange && (
            <label className="flex items-center gap-1.5 cursor-pointer">
              <input
                type="checkbox"
                checked={!hideChangesInFeed}
                onChange={(e) => onHideChangesInFeedChange(!e.target.checked)}
                className="checkbox checkbox-sm"
              />
              Changes
            </label>
          )}
        </div>
      )}
      <div className="flex items-center gap-2 flex-wrap">
      {live ? (
        <>
          <ActionButton
            className="btn btn-info btn-xs"
            busy={busyKey === "action:interrupt"}
            disabled={busy}
            onClick={() => run(() => client.interrupt({ sessionId }), "action:interrupt")}
          >
            Interrupt
          </ActionButton>
          <ActionButton
            className="btn btn-error btn-xs"
            busy={busyKey === "action:kill"}
            disabled={busy}
            onClick={() => run(() => client.stopSession({ sessionId }), "action:kill")}
          >
            Kill
          </ActionButton>
        </>
      ) : (
        // A swept session cannot be warmed: the retention GC removed its
        // working directory and its SDK state, so the pod would come up with
        // nothing to resume. Same reasoning the old proposal guard had — a
        // button that can only ever error is worse than no button.
        //
        // An archived session CAN still be warmed. Archiving says "I'm done
        // with this", not "this is sealed", and reopening it is the natural
        // way to change your mind; ArchiveSession is idempotent either way.
        !sweptAt && (
          <ActionButton
            className="btn btn-success btn-xs"
            busy={busyKey === "action:warm"}
            disabled={busy}
            onClick={() => run(() => client.warmSession({ sessionId }), "action:warm")}
          >
            Warm
          </ActionButton>
        )
      )}
      {/*
        Archive is the human's "I'm finished with this". It is the fleet's
        only terminal state — a polymorphic session (bug fix, explanation,
        dead end) has no completion a machine can compute, which is exactly
        why `done` never got a writer and the status enum went away.

        Nothing offered it until now, so a session could only ever be deleted
        (destroying its transcript) or left to idle forever.
      */}
      {!archivedAt && (
        <ActionButton
          className="btn btn-outline btn-xs"
          busy={busyKey === "action:archive"}
          disabled={busy}
          onClick={() => run(() => client.archiveSession({ sessionId }), "action:archive")}
        >
          Archive
        </ActionButton>
      )}
      {/*
        "Kill e2e" and its "also services" checkbox are gone with the sandbox
        (docs/adr/0048 §6). There is no second pod to kill — the agent's app
        runs in this session's own pod and dies with it — and shared services
        are now requested per session via request_service rather than declared
        per repo, so there is no fleet-wide instance for a human to tear down
        from a session's action bar.
      */}
      {/* Native Popover API + CSS anchor positioning (daisyUI v5's current
          dropdown pattern) instead of the old tabIndex/:focus-within trick —
          only one of these is ever mounted at a time (desktop xor mobile,
          see useMediaQuery in App.tsx), so a static anchor/id pair is safe;
          give it a unique name if a second dropdown ever gets added. */}
      <ActionButton
        className="btn btn-outline btn-xs"
        busy={busyKey === "action:mode"}
        disabled={busy}
        popoverTarget="popover-mode"
        style={{ anchorName: "--anchor-mode" } as CSSProperties}
      >
        Mode: {MODES.find((m) => m.value === currentMode)?.label ?? currentMode ?? "?"} ▾
      </ActionButton>
      <ul
        className="dropdown menu menu-sm bg-base-100 rounded-box shadow w-44 p-1"
        popover="auto"
        id="popover-mode"
        style={{ positionAnchor: "--anchor-mode" } as CSSProperties}
      >
        {MODES.map((m) => (
          <li key={m.value}>
            <button
              type="button"
              className={m.value === currentMode ? "active" : undefined}
              // Dismiss on pick, natively. The mode itself only changes on
              // the wire, and tasks.permissionMode arrives with the next
              // 5s poll — leaving the menu open over an unchanged label is
              // what made this feel like the click was dropped.
              popoverTarget="popover-mode"
              popoverTargetAction="hide"
              onClick={() => run(() => client.setPermissionMode({ sessionId, mode: m.value }), "action:mode")}
            >
              {m.value === currentMode ? "✓ " : ""}
              {m.label}
            </button>
          </li>
        ))}
        <li>
          <button
            type="button"
            className={currentMode === "bypassPermissions" ? "active text-error" : "text-error"}
            onClick={onBypassClick}
          >
            {currentMode === "bypassPermissions" ? "✓ " : ""}
            Bypass permissions
          </button>
        </li>
      </ul>
      {/* The "Open code-server" link is gone with the e2e pod that served it
          (docs/adr/0048 §6). code-server was an IDE surface on the sandbox,
          reached at its /code prefix; a session's pod runs the agent and the
          app, not an IDE. */}
      </div>
    </div>
  );
}
