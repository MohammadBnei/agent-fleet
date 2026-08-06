import type { CSSProperties } from "react";
import { client } from "../connectClient";

// The Approve/Stop/Kill e2e/Mode/Open-code-server button row, shared
// between desktop and mobile — both render it inside a Modal, opened from a
// small icon button rather than either platform picking its own layout.
// hideToolsInFeed/hideChangesInFeed only ever change what mobile's inline
// exchange-zone feed shows (desktop keeps these in dedicated panels
// regardless), but the toggle itself lives here — the one place both
// platforms already have — so both write the same localStorage keys
// instead of each platform getting its own copy of the control. Optional
// only so ActionsMenu doesn't require them if a future caller has no use
// for this toggle.
export function ActionsMenu({
  taskId,
  busy,
  run,
  previewUrl,
  onBypassClick,
  hideToolsInFeed,
  onHideToolsInFeedChange,
  hideChangesInFeed,
  onHideChangesInFeedChange,
}: {
  taskId: string;
  busy: boolean;
  run: (action: () => Promise<unknown>) => void;
  previewUrl: string | null;
  onBypassClick: () => void;
  hideToolsInFeed?: boolean;
  onHideToolsInFeedChange?: (value: boolean) => void;
  hideChangesInFeed?: boolean;
  onHideChangesInFeedChange?: (value: boolean) => void;
}) {
  return (
    <div className="flex flex-col gap-3">
      {(onHideToolsInFeedChange || onHideChangesInFeedChange) && (
        <div className="flex items-center gap-3 text-[12px] text-base-content/70">
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
      <button
        type="button"
        className="btn btn-success btn-xs"
        disabled={busy}
        onClick={() => run(() => client.approve({ taskId }))}
      >
        Approve
      </button>
      <button
        type="button"
        className="btn btn-warning btn-xs"
        disabled={busy}
        onClick={() => run(() => client.stop({ taskId }))}
      >
        Stop
      </button>
      <button
        type="button"
        className="btn btn-outline btn-xs"
        disabled={busy}
        onClick={() => run(() => client.killE2e({ taskId }))}
      >
        Kill e2e
      </button>
      {/* Native Popover API + CSS anchor positioning (daisyUI v5's current
          dropdown pattern) instead of the old tabIndex/:focus-within trick —
          only one of these is ever mounted at a time (desktop xor mobile,
          see useMediaQuery in App.tsx), so a static anchor/id pair is safe;
          give it a unique name if a second dropdown ever gets added. */}
      <button
        type="button"
        className="btn btn-outline btn-xs"
        disabled={busy}
        popoverTarget="popover-mode"
        style={{ anchorName: "--anchor-mode" } as CSSProperties}
      >
        Mode ▾
      </button>
      <ul
        className="dropdown menu menu-sm bg-base-100 rounded-box shadow w-44 p-1"
        popover="auto"
        id="popover-mode"
        style={{ positionAnchor: "--anchor-mode" } as CSSProperties}
      >
        <li>
          <button type="button" onClick={() => run(() => client.setPermissionMode({ taskId, mode: "acceptEdits" }))}>
            Accept edits
          </button>
        </li>
        <li>
          <button type="button" onClick={() => run(() => client.setPermissionMode({ taskId, mode: "dontAsk" }))}>
            Don&apos;t ask
          </button>
        </li>
        <li>
          <button type="button" className="text-error" onClick={onBypassClick}>
            Bypass permissions
          </button>
        </li>
      </ul>
      {previewUrl && (
        <a href={previewUrl} target="_blank" rel="noreferrer" className="btn btn-outline btn-xs">
          Open code-server
        </a>
      )}
      </div>
    </div>
  );
}
