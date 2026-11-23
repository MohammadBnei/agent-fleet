import { type CSSProperties, useEffect, useState } from "react";
import { PERMISSION_MODES } from "../approvePlan";
import { client } from "../connectClient";
import { groupModels } from "../modelRoute";
import { isPodPhaseLive } from "../sessionState";
import { ActionButton } from "./ActionButton";

// The user-facing SDK modes worth a button — "delegate"/"dontAsk" are
// SDK-internal/secondary, never surfaced here, and "bypassPermissions" is
// gone entirely (docs/adr/0053).
//
// `auto` is the one that grants authority: the worker allows everything but
// rm/sudo in it without asking. So it is routed through onAutoClick's confirm
// rather than fired straight off the menu. Every switch is live — nothing
// here costs a re-warm any more.
const MODES = PERMISSION_MODES;

// One button look for the whole row. It was five — info, error, warning,
// success and outline daisyUI fills side by side — so the row read as a
// traffic light rather than a set of actions. Only Stop, the one that ends
// the pod, is marked as different.
const BTN = "btn btn-outline btn-xs font-normal";
const BTN_DANGER = "btn btn-outline btn-error btn-xs font-normal";

// The session's action row — interrupt / stop / restart / warm / archive /
// mode — shared by the desktop SESSION panel, the mobile panels sheet and the
// list's actions modal. The mode dropdown highlights whichever mode is active.
//
// Stop ends the whole pod — a cooperative abort with a grace-period
// force-teardown backstop (DashboardService.StopSession). Interrupt is the
// lighter sibling: it stops only the current turn via the SDK's own
// q.interrupt(), pod and session stay alive — which also cancels a single
// in-flight tool call, since that call *is* the current turn.
export function ActionsMenu({
  sessionId,
  busy,
  isBusy,
  run,
  sweptAt,
  archivedAt,
  currentMode,
  podPhase,
  onAutoClick,
  onRestart,
  liveState,
  currentModel,
  canSetModel,
}: {
  sessionId: string;
  // Whether ANY action anywhere on the page is in flight, not just this
  // menu's own — these buttons are page-level/singular (Stop especially),
  // so they stay conservative and disable together with everything else
  // rather than getting their own independent key.
  busy: boolean;
  // Which one is in flight, so the spinner lands on the button that was
  // actually clicked instead of every button greying out together — the
  // whole point being to tell the clicker their click registered.
  isBusy: (key: string) => boolean;
  run: (action: () => Promise<unknown>, key: string) => void;
  // Set once the retention GC has reclaimed this session's working directory
  // (docs/adr/0048). Readable history, not resumable — so Warm is hidden
  // rather than shown and broken.
  sweptAt?: string;
  // Set when a human marked this session finished — the only terminal state
  // the fleet has, because it is the only one a machine cannot compute.
  archivedAt?: string;
  // Unset for an idle/never-warmed session — no mode has been explicitly
  // chosen yet.
  currentMode?: string;
  // Drives Warm vs. Stop — "is there a pod to talk to right now".
  podPhase?: string;
  onAutoClick: () => void;
  // Sends `/clear` down the normal Discuss path. The worker intercepts it
  // (it can't reach the SDK — streaming-input mode rejects non-prompt
  // commands), clears the saved resume id, and ends the pod; re-warming the
  // session then starts a fresh conversation. Only shown while live.
  onRestart?: () => void;
  // Session.liveState. Interrupt stops the current turn, so it is offered only
  // while there is one — on a finished-but-still-warm session it was a button
  // that could do nothing.
  liveState?: string;
  // Repo sessions only (SetSessionModel refuses a definition session).
  currentModel?: string;
  canSetModel?: boolean;
}) {
  // ListRepos.models is core's pickable list; kill-switch-off hides gateway ids.
  const [models, setModels] = useState<string[]>([]);
  useEffect(() => {
    if (!canSetModel) return;
    client
      .listRepos({})
      .then((r) => setModels(r.models))
      .catch(() => {});
  }, [canSetModel]);
  // A refused switch reads next to the control, not in the generic error modal.
  const [modelErr, setModelErr] = useState("");
  const live = isPodPhaseLive(podPhase);
  const turnRunning = !["done", "idle"].includes(liveState ?? "");
  return (
    <div className="flex items-center gap-2 flex-wrap">
      {live ? (
        <>
          {turnRunning && (
            <ActionButton
              className={BTN}
              busy={isBusy("action:interrupt")}
              disabled={busy}
              onClick={() =>
                run(() => client.interrupt({ sessionId }), "action:interrupt")
              }
            >
              interrupt
            </ActionButton>
          )}
          <ActionButton
            className={BTN_DANGER}
            busy={isBusy("action:stop")}
            disabled={busy}
            title="End the pod. The session stays and can be warmed again."
            onClick={() =>
              run(() => client.stopSession({ sessionId }), "action:stop")
            }
          >
            stop
          </ActionButton>
          {onRestart && (
            <ActionButton
              className={BTN}
              busy={isBusy("discuss")}
              disabled={busy}
              onClick={onRestart}
              title="Send /clear — end this conversation and start fresh on the next warm"
            >
              restart
            </ActionButton>
          )}
        </>
      ) : (
        // A swept session cannot be warmed: the retention GC removed its
        // working directory and its SDK state, so the pod would come up with
        // nothing to resume. An archived one is finished: no warm.
        !sweptAt &&
        !archivedAt && (
          <ActionButton
            className={BTN}
            busy={isBusy("action:warm")}
            disabled={busy}
            title="Start a pod and resume this session"
            onClick={() =>
              run(() => client.warmSession({ sessionId }), "action:warm")
            }
          >
            warm
          </ActionButton>
        )
      )}
      {/* Archive is the human's "I'm finished with this" — the fleet's only
          terminal state, because a polymorphic session has no completion a
          machine can compute. */}
      {!archivedAt && (
        <ActionButton
          className={BTN}
          busy={isBusy("action:archive")}
          disabled={busy}
          onClick={() =>
            run(() => client.archiveSession({ sessionId }), "action:archive")
          }
        >
          archive
        </ActionButton>
      )}
      {/* Native Popover API + CSS anchor positioning. Only one ActionsMenu is
          mounted at a time (desktop xor mobile, or the list's modal while no
          detail is open), so a static anchor/id pair is safe. */}
      {/* An archived session is finished: no model, no warm, no mode. */}
      {!archivedAt && (
        <>
          <ActionButton
            className={BTN}
            busy={isBusy("action:mode")}
            disabled={busy}
            popoverTarget="popover-mode"
            style={{ anchorName: "--anchor-mode" } as CSSProperties}
          >
            mode:{" "}
            {(
              MODES.find((m) => m.value === currentMode)?.label ??
              currentMode ??
              "?"
            ).toLowerCase()}{" "}
            ▾
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
                  // Dismiss on pick, natively. The mode only changes on the wire,
                  // and leaving the menu open over an unchanged label is what made
                  // this feel like the click was dropped.
                  popoverTarget="popover-mode"
                  popoverTargetAction="hide"
                  onClick={() =>
                    "confirm" in m && m.confirm
                      ? onAutoClick()
                      : run(
                          () =>
                            client.setPermissionMode({
                              sessionId,
                              mode: m.value,
                            }),
                          "action:mode",
                        )
                  }
                >
                  {m.value === currentMode ? "✓ " : ""}
                  {m.label}
                </button>
              </li>
            ))}
          </ul>
        </>
      )}
      {canSetModel && !archivedAt && models.length > 0 && (
        <>
          <ActionButton
            className={BTN}
            busy={isBusy("action:model")}
            disabled={busy}
            title="Stops the pod; the next message warms on the new model"
            popoverTarget="popover-model"
            style={{ anchorName: "--anchor-model" } as CSSProperties}
          >
            model: {currentModel || "default"} ▾
          </ActionButton>
          <ul
            className="dropdown menu menu-sm bg-base-100 rounded-box shadow w-64 p-1"
            popover="auto"
            id="popover-model"
            style={{ positionAnchor: "--anchor-model" } as CSSProperties}
          >
            {groupModels(models).flatMap((g) => [
              <li key={g.route} className="menu-title">
                {g.label}
              </li>,
              ...g.models.map((m) => (
                <li key={m}>
                  <button
                    type="button"
                    className={m === currentModel ? "active" : undefined}
                    popoverTarget="popover-model"
                    popoverTargetAction="hide"
                    onClick={() =>
                      run(async () => {
                        setModelErr("");
                        try {
                          await client.setSessionModel({ sessionId, model: m });
                        } catch (e) {
                          setModelErr(
                            (e as Error).message.replace(/^\[[a-z_]+\]\s*/, ""),
                          );
                        }
                      }, "action:model")
                    }
                  >
                    {m === currentModel ? "✓ " : ""}
                    {m}
                  </button>
                </li>
              )),
            ])}
          </ul>
          {modelErr && (
            <span role="alert" className="basis-full text-xs text-error">
              {modelErr}
            </span>
          )}
        </>
      )}
    </div>
  );
}
