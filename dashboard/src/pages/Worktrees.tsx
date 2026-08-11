import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import type { WorktreeView } from "../gen/agentfleet/v1/dashboard_pb";
import { ErrorModal } from "../components/ErrorModal";
import { ConfirmModal } from "../components/ConfirmModal";

// The manual half of worktree/branch cleanup (reliability-findings.md #2)
// — the automated half is the provisioner's periodic [gone]-branch sweep,
// which can't reach a worktree abandoned before its first push (no
// upstream ref to compare against). Rows with no taskStatus are exactly
// that case: an orphaned worktree with no task row left to explain it.

// Terminal task statuses, mirroring core/internal/coreserver/server.go's
// terminalTaskStatuses (itself mirroring db/migrations' tasks_status_check).
const TERMINAL_STATUSES = new Set(["done", "failed", "cancelled", "failed_permanently"]);

// ponytail: a 2-minute mtime grace instead of plumbing tasks.pod_phase all
// the way into WorktreeView. A worker sets its terminal status just before
// exiting, so a pod can still be mid-`git push`/`gh pr create` when its task
// already reads "done" — yanking the checkout out from under it there would
// cost the PR. Anything touched recently is left for the next click. Wire up
// pod_phase if this grace ever proves too coarse.
const RECENT_SECS = 120;

// A worktree is stale once its task is finished (or its task row is gone
// entirely) and nothing has written to it recently. Exported for its test.
export function isStaleWorktree(w: WorktreeView, nowSecs: number): boolean {
  if (w.taskStatus !== undefined && !TERMINAL_STATUSES.has(w.taskStatus)) return false;
  return nowSecs - Number(w.mtimeUnix) > RECENT_SECS;
}

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
  const [confirmOpen, setConfirmOpen] = useState(false);
  const orphaned = worktree.taskStatus === undefined;
  const what = alsoDeleteBranch ? "worktree and branch" : "worktree";

  async function handleDelete() {
    setConfirmOpen(false);
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
        <button type="button" onClick={() => setConfirmOpen(true)} disabled={deleting} className="btn btn-xs btn-error">
          {deleting ? "Deleting…" : "Delete"}
        </button>
        <ConfirmModal
          open={confirmOpen}
          message={`Delete the ${what} for ${worktree.branch}?`}
          onConfirm={handleDelete}
          onCancel={() => setConfirmOpen(false)}
        />
      </td>
    </tr>
  );
}

// onBack is only passed by the mobile wrapper — desktop's grid column has
// no "back" concept, it's a permanent third top-level view.
export function Worktrees({ onBack }: { onBack?: () => void } = {}) {
  const [worktrees, setWorktrees] = useState<WorktreeView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [syncConfirmOpen, setSyncConfirmOpen] = useState(false);

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

  const stale = worktrees.filter((w) => isStaleWorktree(w, Date.now() / 1000));

  async function handleSync() {
    setSyncConfirmOpen(false);
    setSyncing(true);
    setError(null);
    try {
      // Sequential, not Promise.all: the provisioner serializes every git
      // mutation behind one per-repo mutex anyway, so concurrency buys
      // nothing here and only makes a partial failure harder to report.
      // alsoDeleteBranch stays false — freeing the checkout must never be
      // what destroys the only reference to unpushed commits
      // (reliability-findings.md #2). Branches are the periodic sweep's job.
      for (const w of stale) {
        await client.deleteWorktree({ taskId: w.taskId, repo: w.repo, alsoDeleteBranch: false });
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setSyncing(false);
    await load();
  }

  return (
    <div className="lg:col-span-2 min-h-0 overflow-y-auto p-4 overflow-x-auto">
      <div className="flex items-center gap-3 mb-3">
        {onBack && (
          <button type="button" onClick={onBack} className="text-[17px] text-base-content/60 w-7 h-8 flex items-center">
            ‹
          </button>
        )}
        <h2 className="font-semibold text-base">Worktrees</h2>
        <button type="button" onClick={load} disabled={loading} className="btn btn-xs">
          {loading ? "Refreshing…" : "Refresh"}
        </button>
        <button
          type="button"
          onClick={() => setSyncConfirmOpen(true)}
          disabled={loading || syncing || stale.length === 0}
          className="btn btn-xs btn-warning"
          title="Remove the on-disk checkouts of finished and orphaned tasks (branches are kept)"
        >
          {syncing ? "Syncing…" : `Sync with git${stale.length > 0 ? ` (${stale.length})` : ""}`}
        </button>
      </div>
      <ConfirmModal
        open={syncConfirmOpen}
        title="Sync with git"
        message={`Remove ${stale.length} stale worktree${stale.length === 1 ? "" : "s"} (${stale
          .map((w) => w.branch)
          .join(", ")})? The branches are kept, so no commits are lost — only the on-disk checkouts are freed.`}
        confirmLabel="Remove"
        onConfirm={handleSync}
        onCancel={() => setSyncConfirmOpen(false)}
      />
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
