import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import type { WorktreeView } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "../components/InlineError";
import { ConfirmModal } from "../components/ConfirmModal";
import { useMediaQuery } from "../useMediaQuery";

// The manual half of worktree/branch cleanup (reliability-findings.md #2) — the
// automated half is the provisioner's periodic [gone]-branch sweep, which can't
// reach a worktree abandoned before its first push (no upstream ref to compare
// against). Rows with no taskStatus are exactly that case: an orphan with no
// task row left to explain it.

// Live states in which nothing is using this worktree, from
// core/internal/sessions/liveness.go's vocabulary.
//
// This set used to hold the task statuses "done"/"failed"/"cancelled"/
// "failed_permanently". Three of those no longer exist (docs/adr/0048 deleted
// the enum), and the miss was not cosmetic: a stopped session's live state is
// `""` — no live pod, the resting state of every session between messages —
// so with only "done" matching, no worktree was ever reported reclaimable
// again. The view kept showing them as in-use forever.
//
// `blocked` and `stalled` are deliberately absent: both still have a live pod
// with an agent attached, waiting on a human rather than finished.
const TERMINAL_STATUSES = new Set(["done", ""]);

// ponytail: a 2-minute mtime grace instead of plumbing pod_phase all the way
// into WorktreeView. A pod can still be mid-`git push`/`gh pr create` when its
// session already reads as having no live pod — yanking the checkout out from
// under it there would cost the PR. Anything touched recently is left for the
// next click. Wire up pod_phase if this grace ever proves too coarse.
const RECENT_SECS = 120;

const COLS = "grid-cols-[20px_1.4fr_130px_1fr_150px_90px_110px_90px]";

// A worktree is stale once its task is finished (or its task row is gone
// entirely) and nothing has written to it recently. Exported for its test.
export function isStaleWorktree(w: WorktreeView, nowSecs: number): boolean {
  if (w.liveState !== undefined && !TERMINAL_STATUSES.has(w.liveState)) return false;
  return nowSecs - Number(w.mtimeUnix) > RECENT_SECS;
}

export function formatBytes(bytes: bigint | number): string {
  const n = Number(bytes);
  if (n <= 0) return "—";
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(0)} KB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(0)} MB`;
  return `${(n / 1024 ** 3).toFixed(1)} GB`;
}

function relativeMtime(mtimeUnix: bigint): string {
  if (mtimeUnix === 0n) return "—";
  const secs = Math.max(0, Date.now() / 1000 - Number(mtimeUnix));
  if (secs < 60) return `${Math.round(secs)}s ago`;
  const mins = secs / 60;
  if (mins < 60) return `${Math.round(mins)}m ago`;
  const hours = mins / 60;
  if (hours < 24) return `${Math.round(hours)}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

// Why this worktree exists and whether it's safe to remove. Exported for its
// test: it decides whether a row reads as an orphan and whether it links to a
// live session, and calling a still-running worktree an orphan is how someone
// ends up deleting work in progress.
export function owner(w: WorktreeView): { label: string; cls: string; orphan: boolean } {
  if (w.liveState === undefined) {
    return { label: "orphan · no session", cls: "text-warning", orphan: true };
  }
  const live = !TERMINAL_STATUSES.has(w.liveState);
  return {
    label: `#${w.sessionId.slice(0, 6)} ${w.liveState}`,
    cls: live ? "text-error" : "text-dim",
    orphan: false,
  };
}

function DeleteWorktree({
  worktree,
  onDeleted,
  onError,
}: {
  worktree: WorktreeView;
  onDeleted: () => void;
  onError: (msg: string) => void;
}) {
  const [alsoDeleteBranch, setAlsoDeleteBranch] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const dirty = worktree.dirtyFiles > 0;

  async function handleDelete() {
    setConfirmOpen(false);
    setDeleting(true);
    try {
      await client.deleteWorktree({ sessionId: worktree.sessionId, repo: worktree.repo, alsoDeleteBranch });
      onDeleted();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
      setDeleting(false);
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={() => setConfirmOpen(true)}
        disabled={deleting}
        className={`border px-2.5 py-1 text-xs disabled:opacity-50 ${
          dirty ? "border-orange-line text-warning" : "border-line text-dim hover:text-error"
        }`}
      >
        {deleting ? "deleting…" : "delete"}
      </button>
      <ConfirmModal
        open={confirmOpen}
        title="Delete worktree?"
        message={
          dirty
            ? `${worktree.branch} has ${worktree.dirtyFiles} uncommitted file${
                worktree.dirtyFiles === 1 ? "" : "s"
              }. Deleting the worktree throws that work away.${
                alsoDeleteBranch ? " The branch goes too, so any unpushed commits go with it." : " The branch is kept."
              }`
            : `Delete the ${alsoDeleteBranch ? "worktree and branch" : "worktree"} for ${worktree.branch}?`
        }
        confirmLabel="Delete"
        danger={dirty}
        onConfirm={handleDelete}
        onCancel={() => setConfirmOpen(false)}
      />
      <label className="flex items-center gap-1 text-2xs text-dim2 whitespace-nowrap cursor-pointer">
        <input
          type="checkbox"
          checked={alsoDeleteBranch}
          onChange={(e) => setAlsoDeleteBranch(e.target.checked)}
          className="checkbox checkbox-xs"
        />
        + branch
      </label>
    </>
  );
}

export function Worktrees({ onSelectTask }: { onSelectTask: (id: string) => void }) {
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [worktrees, setWorktrees] = useState<WorktreeView[]>([]);
  const [pvc, setPvc] = useState<{ total: number; free: number }>({ total: 0, free: 0 });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [syncConfirmOpen, setSyncConfirmOpen] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    return client
      .listWorktrees({})
      .then((res) => {
        setWorktrees(res.worktrees);
        setPvc({ total: Number(res.pvcTotalBytes), free: Number(res.pvcFreeBytes) });
      })
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const stale = worktrees.filter((w) => isStaleWorktree(w, Date.now() / 1000));
  const used = pvc.total - pvc.free;
  const usedPct = pvc.total > 0 ? Math.min(100, Math.round((used / pvc.total) * 100)) : 0;

  async function handleSync() {
    setSyncConfirmOpen(false);
    setSyncing(true);
    setError(null);
    try {
      // Sequential, not Promise.all: the provisioner serializes every git
      // mutation behind one per-repo mutex anyway, so concurrency buys nothing
      // and only makes a partial failure harder to report. alsoDeleteBranch
      // stays false — freeing the checkout must never be what destroys the only
      // reference to unpushed commits (reliability-findings.md #2). Branches are
      // the periodic sweep's job.
      for (const w of stale) {
        await client.deleteWorktree({ sessionId: w.sessionId, repo: w.repo, alsoDeleteBranch: false });
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
    setSyncing(false);
    await load();
  }

  const header = (
    <div className="flex items-baseline gap-3 flex-wrap">
      <h2 className="text-base font-semibold">Worktrees</h2>
      <span className="text-xs text-dim2">
        {isDesktop && "git worktrees on the shared volume · "}
        {worktrees.length} tree{worktrees.length === 1 ? "" : "s"}
        {pvc.total > 0 && ` · ${formatBytes(used)} of ${formatBytes(pvc.total)}`}
      </span>
      {pvc.total > 0 && (
        <div className="w-[200px] max-w-full h-1 bg-line3 flex-none" title={`${usedPct}% used`}>
          <div className={`h-1 ${usedPct > 90 ? "bg-warning" : "bg-primary"}`} style={{ width: `${usedPct}%` }} />
        </div>
      )}
      <div className="ml-auto flex gap-2">
        <button
          type="button"
          onClick={() => setSyncConfirmOpen(true)}
          disabled={loading || syncing || stale.length === 0}
          title="Remove the on-disk checkouts of finished and orphaned tasks (branches are kept)"
          className="border border-acc-line px-3 py-1.5 text-xs hover:border-primary hover:text-primary disabled:opacity-40"
        >
          {syncing ? "pruning…" : `prune orphans (${stale.length})`}
        </button>
        <button
          type="button"
          onClick={load}
          disabled={loading}
          className="border border-line px-3 py-1.5 text-xs text-dim hover:text-base-content disabled:opacity-40"
        >
          {loading ? "…" : "refresh"}
        </button>
      </div>
    </div>
  );

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-3.5 sm:px-4.5 pt-4 sm:pt-5 pb-6 flex flex-col gap-3.5">
      <InlineError message={error} onRetry={load} onDismiss={() => setError(null)} />
      <ConfirmModal
        open={syncConfirmOpen}
        title="Prune orphaned worktrees"
        message={`Remove ${stale.length} stale worktree${stale.length === 1 ? "" : "s"} (${stale
          .map((w) => w.branch)
          .join(", ")})? The branches are kept, so no commits are lost — only the on-disk checkouts are freed.`}
        confirmLabel="Remove"
        onConfirm={handleSync}
        onCancel={() => setSyncConfirmOpen(false)}
      />
      {header}

      {!loading && worktrees.length === 0 && !error && (
        <div className="text-sm text-dim2">No worktrees on disk.</div>
      )}

      {worktrees.length > 0 &&
        (isDesktop ? (
          <div className="border border-line2">
            <div className={`grid ${COLS} gap-3.5 px-3.5 py-2 border-b border-line text-2xs tracking-[0.1em] text-dim2`}>
              <div />
              <div>PATH</div>
              <div>REPO</div>
              <div>BRANCH</div>
              <div>OWNER</div>
              <div>SIZE</div>
              <div>TOUCHED</div>
              <div />
            </div>
            {worktrees.map((w, i) => {
              const o = owner(w);
              return (
                <div
                  key={`${w.repo}/${w.sessionId}`}
                  className={`grid ${COLS} gap-3.5 px-3.5 py-2.5 items-center ${
                    i === worktrees.length - 1 ? "" : "border-b border-line3"
                  } ${o.orphan ? "bg-orange-wash" : ""}`}
                >
                  <span
                    className={`w-[7px] h-[7px] rounded-full flex-none ${
                      o.orphan ? "bg-warning" : TERMINAL_STATUSES.has(w.liveState ?? "") ? "bg-success" : "bg-info"
                    }`}
                  />
                  <div className="text-sm min-w-0 truncate" title={w.path}>
                    {w.path || "—"}
                  </div>
                  <div className="text-sm text-dim min-w-0 truncate">{w.repo}</div>
                  <div className="text-sm text-text2 min-w-0 truncate" title={w.branch}>
                    {w.branch}{" "}
                    {w.dirtyFiles > 0 ? (
                      <span className="text-warning">·{w.dirtyFiles} dirty</span>
                    ) : (
                      <span className="text-dim2">·clean</span>
                    )}
                  </div>
                  <div className="min-w-0 truncate">
                    {o.orphan ? (
                      <span className={`text-sm ${o.cls}`}>{o.label}</span>
                    ) : (
                      <button
                        type="button"
                        onClick={() => onSelectTask(w.sessionId)}
                        className={`text-sm ${o.cls} hover:text-primary cursor-pointer`}
                      >
                        {o.label} ▸
                      </button>
                    )}
                    {w.sessionError && (
                      <div className="text-xs text-warning truncate" title={w.sessionError}>
                        {w.sessionError}
                      </div>
                    )}
                  </div>
                  <div className="text-sm text-dim">{formatBytes(w.sizeBytes)}</div>
                  <div className="text-sm text-dim">{relativeMtime(w.mtimeUnix)}</div>
                  <div className="flex gap-1.5 justify-end items-center flex-wrap">
                    {isStaleWorktree(w, Date.now() / 1000) ? (
                      <DeleteWorktree worktree={w} onDeleted={load} onError={setError} />
                    ) : (
                      <span className="text-xs text-dim2">in use</span>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        ) : (
          worktrees.map((w) => {
            const o = owner(w);
            return (
              <div
                key={`${w.repo}/${w.sessionId}`}
                className={`border px-3.5 py-3 ${o.orphan ? "border-orange-line bg-orange-bg" : "border-line2"}`}
              >
                <div className="flex items-center gap-2">
                  <span
                    className={`w-1.5 h-1.5 rounded-full flex-none ${
                      o.orphan ? "bg-warning" : TERMINAL_STATUSES.has(w.liveState ?? "") ? "bg-success" : "bg-info"
                    }`}
                  />
                  <span className="text-sm min-w-0 truncate" title={w.path}>
                    {w.path || w.branch}
                  </span>
                  <span className="text-xs text-dim2 ml-auto flex-none">{formatBytes(w.sizeBytes)}</span>
                </div>
                <div className="text-xs text-text2 mt-1.5 min-w-0 truncate">
                  {w.branch}{" "}
                  {w.dirtyFiles > 0 ? (
                    <span className="text-warning">· {w.dirtyFiles} dirty</span>
                  ) : (
                    <span className="text-dim2">· clean</span>
                  )}
                </div>
                <div className="flex items-center gap-2 mt-2 flex-wrap">
                  {o.orphan ? (
                    <span className={`text-xs ${o.cls}`}>{o.label}</span>
                  ) : (
                    <button
                      type="button"
                      onClick={() => onSelectTask(w.sessionId)}
                      className={`text-xs ${o.cls}`}
                    >
                      {o.label} ▸
                    </button>
                  )}
                  <span className="text-xs text-dim2">{relativeMtime(w.mtimeUnix)}</span>
                  <div className="ml-auto flex items-center gap-2">
                    {isStaleWorktree(w, Date.now() / 1000) ? (
                      <DeleteWorktree worktree={w} onDeleted={load} onError={setError} />
                    ) : (
                      <span className="text-xs text-dim2">in use</span>
                    )}
                  </div>
                </div>
              </div>
            );
          })
        ))}

      {worktrees.length > 0 && (
        <div className="text-xs text-dim2 leading-[1.6]">
          An orphan is a tree whose session is gone. Deleting one drops uncommitted work — the dirty count is the
          warning.
        </div>
      )}
    </div>
  );
}
