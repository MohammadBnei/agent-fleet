import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import type { WorktreeView } from "../gen/agentfleet/v1/dashboard_pb";
import { ErrorModal } from "../components/ErrorModal";

// The manual half of worktree/branch cleanup (reliability-findings.md #2)
// — the automated half is the provisioner's periodic [gone]-branch sweep,
// which can't reach a worktree abandoned before its first push (no
// upstream ref to compare against). Rows with no taskStatus are exactly
// that case: an orphaned worktree with no task row left to explain it.
function formatMtime(mtimeUnix: bigint): string {
  if (mtimeUnix === 0n) return "—";
  return new Date(Number(mtimeUnix) * 1000).toLocaleString();
}

function WorktreeRow({
  worktree,
  onDeleted,
}: {
  worktree: WorktreeView;
  onDeleted: () => void;
}) {
  const [alsoDeleteBranch, setAlsoDeleteBranch] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const orphaned = worktree.taskStatus === undefined;

  async function handleDelete() {
    const what = alsoDeleteBranch ? "worktree and branch" : "worktree";
    if (!window.confirm(`Delete the ${what} for ${worktree.branch}?`)) return;
    setDeleting(true);
    setError(null);
    try {
      await client.deleteWorktree({
        taskId: worktree.taskId,
        repo: worktree.repo,
        alsoDeleteBranch,
      });
      onDeleted();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  }

  return (
    <tr className={orphaned ? "bg-warning/10" : ""}>
      <td className="text-[11px]">{worktree.repo}</td>
      <td className="text-[11px] font-mono">{worktree.branch}</td>
      <td className="text-[11px]">{worktree.upstreamTrack || "—"}</td>
      <td className="text-[11px]">{formatMtime(worktree.mtimeUnix)}</td>
      <td className="text-[11px]">
        {orphaned ? <span className="text-warning">orphaned</span> : worktree.taskStatus}
        {worktree.taskError && (
          <div className="text-error text-[10px] max-w-64 truncate" title={worktree.taskError}>
            {worktree.taskError}
          </div>
        )}
        {worktree.prUrl && (
          <a href={worktree.prUrl} target="_blank" rel="noreferrer" className="link text-[10px] block">
            PR
          </a>
        )}
      </td>
      <td className="text-right align-top">
        {error && <div className="text-error text-[10px] mb-1">{error}</div>}
        <label className="flex items-center gap-1 text-[10px] justify-end mb-1 whitespace-nowrap">
          <input
            type="checkbox"
            checked={alsoDeleteBranch}
            onChange={(e) => setAlsoDeleteBranch(e.target.checked)}
            className="checkbox checkbox-xs"
          />
          also delete branch
        </label>
        <button type="button" onClick={handleDelete} disabled={deleting} className="btn btn-xs btn-error">
          {deleting ? "Deleting…" : "Delete"}
        </button>
      </td>
    </tr>
  );
}

export function Worktrees() {
  const [worktrees, setWorktrees] = useState<WorktreeView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    return client
      .listWorktrees({})
      .then((res) => setWorktrees(res.worktrees))
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="lg:col-span-2 min-h-0 overflow-y-auto p-4 overflow-x-auto">
      <div className="flex items-center gap-3 mb-3">
        <h2 className="font-semibold text-base">Worktrees</h2>
        <button type="button" onClick={load} disabled={loading} className="btn btn-xs">
          {loading ? "Refreshing…" : "Refresh"}
        </button>
      </div>
      <ErrorModal message={error} onClose={() => setError(null)} />
      {!loading && worktrees.length === 0 && !error && (
        <div className="opacity-60 text-sm">No worktrees on disk.</div>
      )}
      {worktrees.length > 0 && (
        <table className="table table-sm">
          <thead>
            <tr className="text-[10px]">
              <th>Repo</th>
              <th>Branch</th>
              <th>Upstream</th>
              <th>Modified</th>
              <th>Task</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {worktrees.map((w) => (
              <WorktreeRow key={`${w.repo}/${w.taskId}`} worktree={w} onDeleted={load} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
