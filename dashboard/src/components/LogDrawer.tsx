import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { LogEntry } from "../gen/agentfleet/v1/core_pb";
import { Modal } from "./Modal";

// "read log" on a failed session. DashboardService.QueryLogs has existed since
// the Loki client landed and had no UI caller at all — the reason a session had
// gone quiet was only reachable by someone with cluster access. `task_id` is a
// real Loki stream label (Alloy promotes the agent-fleet.dev/task-id pod
// label), so a per-task query genuinely works, including for the e2e/app pods
// that don't log JSON.

const LEVEL_COLOR: Record<string, string> = {
  error: "text-error",
  warn: "text-warning",
  info: "text-dim",
  debug: "text-dim2",
};

export function LogDrawer({ taskId, onClose }: { taskId: string | null; onClose: () => void }) {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!taskId) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setEntries([]);
    client
      .queryLogs({ taskId, limit: 200 })
      .then((res) => {
        if (!cancelled) setEntries(res.entries);
      })
      .catch((err: Error) => {
        if (!cancelled) setError(err.message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [taskId]);

  return (
    <Modal open={taskId !== null} onClose={onClose} boxClassName="max-w-4xl">
      <h3 className="text-base font-semibold mb-1">Logs · #{taskId?.slice(0, 6)}</h3>
      <p className="text-xs text-dim2 mb-3">
        Newest 200 lines from Loki for this session's pods.
      </p>
      {loading && <div className="text-sm text-dim">Loading…</div>}
      {error && <div className="text-sm text-error">{error}</div>}
      {!loading && !error && entries.length === 0 && (
        <div className="text-sm text-dim2">
          No log lines for this session — its pod may already be gone past Loki's retention.
        </div>
      )}
      {entries.length > 0 && (
        <div className="border border-line bg-code max-h-[60vh] overflow-auto">
          {entries.map((e, i) => (
            <div key={i} className="flex gap-2.5 px-2.5 py-[3px] text-xs whitespace-pre-wrap break-all">
              <span className="text-dim2 flex-none">{e.timestamp?.slice(11, 19) || "—"}</span>
              <span className={`flex-none w-10 ${LEVEL_COLOR[e.level?.toLowerCase()] ?? "text-dim2"}`}>
                {e.level || "—"}
              </span>
              <span className="text-dim2 flex-none w-24 truncate" title={e.component}>
                {e.component}
              </span>
              <span className="text-text2 min-w-0">{e.msg}</span>
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}
