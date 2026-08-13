import { useCallback, useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import type { GetE2eStatusResponse } from "../gen/agentfleet/v1/dashboard_pb";
import { Modal } from "./Modal";

// Full control over one task's e2e environment, opened from the E2E card in
// the detail view's panel column. It's a drawer rather than more rows on that
// 266px card for one reason: the app log. A log at 266px wraps every line
// three times and is unreadable, and reading the log is the whole point —
// "why isn't my preview serving" was previously answerable only by someone
// with cluster access (docs/adr/0044).
//
// The distinction the buttons encode is the one ADR-0044 drew: restarting the
// APP re-runs the start command inside the live pod and costs seconds;
// recreating the POD throws away the warm dependency cache and costs a 10+
// minute cold install. One "Restart" button covering both is how a human pays
// that install to fix a dev server that just needed rebooting.

type Busy = "start" | "restart" | "stop" | "log" | null;

export function E2eManageDrawer({
  taskId,
  e2e,
  open,
  onClose,
  onChanged,
}: {
  taskId: string;
  e2e: GetE2eStatusResponse | null;
  open: boolean;
  onClose: () => void;
  // Nudges the parent's 5s status poll so the card doesn't sit on a stale
  // phase for up to five seconds after a click that just changed it.
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState<Busy>(null);
  const [error, setError] = useState<string | null>(null);
  const [log, setLog] = useState<string>("");
  const [logLoaded, setLogLoaded] = useState(false);
  const [confirmRecreate, setConfirmRecreate] = useState(false);
  const logRef = useRef<HTMLPreElement>(null);

  const podLive = e2e?.podPhase === "Running";
  const hasPod = Boolean(e2e?.status);
  const sandboxOnly = !e2e?.startCmd;

  const refreshLog = useCallback(async () => {
    setBusy("log");
    setError(null);
    try {
      const res = await client.getE2eAppLog({ taskId, lines: 300 });
      setLog(res.log);
      setLogLoaded(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  }, [taskId]);

  // Load the log once on open, not on a timer: a poll here would run a
  // `tail` in the pod every few seconds for a drawer that is usually open for
  // a few seconds. Refresh is a button.
  useEffect(() => {
    if (!open || !podLive || logLoaded) return;
    void refreshLog();
  }, [open, podLive, logLoaded, refreshLog]);

  // Stick to the bottom — a log you have to scroll to the end of every time
  // buries the exit-status marker, which is the line that matters most.
  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [log]);

  useEffect(() => {
    if (!open) {
      setError(null);
      setConfirmRecreate(false);
      setLogLoaded(false);
    }
  }, [open]);

  async function act(key: Exclude<Busy, null>, fn: () => Promise<unknown>) {
    setBusy(key);
    setError(null);
    try {
      await fn();
      onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  }

  const start = () => act("start", () => client.startE2e({ taskId }));
  const stop = () => act("stop", () => client.killE2e({ taskId, alsoTeardownServices: false }));
  const restartApp = () =>
    act("restart", async () => {
      const res = await client.restartE2eApp({ taskId });
      setLog((prev) => `${prev}\n${res.output}`.trim());
      // The app takes a moment to bind; re-read rather than leaving the
      // restart's own output as the last thing shown.
      setTimeout(() => void refreshLog(), 1500);
    });
  const recreate = () =>
    act("start", async () => {
      await client.killE2e({ taskId, alsoTeardownServices: false });
      await client.startE2e({ taskId });
      setLog("");
      setLogLoaded(false);
    });

  return (
    <Modal open={open} onClose={onClose} boxClassName="max-w-3xl">
      <h3 className="text-base font-semibold mb-1">E2E environment · #{taskId.slice(0, 6)}</h3>
      <p className="text-xs text-dim2 mb-3">
        {hasPod ? (
          <>
            pod <span className="text-text2">{e2e?.podPhase || "—"}</span> · profile{" "}
            <span className="text-text2">{e2e?.profileName || "none"}</span>
            {sandboxOnly && " · no app command (build/test sandbox only)"}
          </>
        ) : (
          "No sandbox running for this task."
        )}
      </p>

      <div className="flex flex-wrap gap-2 mb-3">
        {e2e?.previewUrl && e2e.appReady && (
          <a href={e2e.previewUrl} target="_blank" rel="noreferrer" className="btn btn-outline btn-xs">
            Open app
          </a>
        )}
        {e2e?.codeServerUrl && podLive && (
          <a href={e2e.codeServerUrl} target="_blank" rel="noreferrer" className="btn btn-outline btn-xs">
            Open VS Code
          </a>
        )}
      </div>

      <div className="border-t border-line pt-3 mb-3">
        <div className="text-2xs text-dim2 uppercase tracking-wide mb-2">Lifecycle</div>
        <div className="flex flex-wrap gap-2 items-center">
          {!hasPod && (
            <button type="button" className="btn btn-xs btn-primary" disabled={busy !== null} onClick={start}>
              {busy === "start" ? "Starting…" : "Start sandbox"}
            </button>
          )}
          {podLive && !sandboxOnly && (
            <button type="button" className="btn btn-xs" disabled={busy !== null} onClick={restartApp}>
              {busy === "restart" ? "Restarting…" : "Restart app"}
            </button>
          )}
          {hasPod && (
            <button type="button" className="btn btn-xs btn-error" disabled={busy !== null} onClick={stop}>
              {busy === "stop" ? "Stopping…" : "Stop"}
            </button>
          )}
          {hasPod &&
            (confirmRecreate ? (
              <span className="flex items-center gap-1.5">
                <button type="button" className="btn btn-xs btn-error" disabled={busy !== null} onClick={recreate}>
                  {busy === "start" ? "Recreating…" : "Yes, recreate"}
                </button>
                <button type="button" className="btn btn-xs btn-ghost" onClick={() => setConfirmRecreate(false)}>
                  Cancel
                </button>
              </span>
            ) : (
              <button type="button" className="btn btn-xs btn-outline" onClick={() => setConfirmRecreate(true)}>
                Recreate pod…
              </button>
            ))}
        </div>
        {podLive && !sandboxOnly && (
          <p className="text-2xs text-dim2 mt-2 leading-snug">
            <strong className="text-dim">Restart app</strong> re-runs the start command in the live pod — seconds, keeps
            the warm dependency cache. <strong className="text-dim">Recreate pod</strong> throws that cache away and
            costs a cold install, 10+ minutes.
          </p>
        )}
        {confirmRecreate && (
          <p className="text-2xs text-warning mt-2 leading-snug">
            This deletes the pod and builds a new one. Dependencies install from cold — expect 10+ minutes before the
            preview serves again. Uncommitted work in the worktree is not affected.
          </p>
        )}
      </div>

      <div className="border-t border-line pt-3">
        <div className="flex items-center gap-2 mb-2">
          <span className="text-2xs text-dim2 uppercase tracking-wide">App log</span>
          <span className="text-2xs text-dim2">/tmp/e2e-app.log</span>
          <button
            type="button"
            className="btn btn-2xs btn-ghost ml-auto"
            disabled={!podLive || busy !== null}
            onClick={refreshLog}
          >
            {busy === "log" ? "Reading…" : "Refresh"}
          </button>
        </div>
        {!podLive ? (
          <div className="text-xs text-dim2">The pod isn't running, so there's no log to read.</div>
        ) : (
          <pre
            ref={logRef}
            className="border border-line bg-code text-xs text-text2 p-2.5 max-h-[40vh] overflow-auto whitespace-pre-wrap [overflow-wrap:anywhere]"
          >
            {log || (logLoaded ? "(empty)" : "Loading…")}
          </pre>
        )}
      </div>

      {error && <p className="text-error text-sm mt-3">{error}</p>}
    </Modal>
  );
}
