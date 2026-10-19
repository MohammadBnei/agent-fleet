import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import type { Repo, SyncRepoResponse } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// The fleet's target repos (docs/adr/0028), projected from GitHub plus
// infra-bootstrap's overrides file since docs/adr/0074.

// What a finished sync says. The point of the button is answering "did that
// do anything?", so a bare "done" would waste the whole round trip — the
// provisioner already counted the commits and timed itself.
//
// "already current" is the one claim here that can be wrong, so it is the last
// branch rather than the fallback. commitsAdvanced is 0 in two cases that are
// not "nothing moved": no resolvable origin/HEAD to measure against (which is
// every cache cloned before that symref was recorded, i.e. exactly the stale
// ones someone is most likely to sync), and a force-push that rewound the
// branch. Both are reported as movement of unknown size instead.
function syncLabel(res: SyncRepoResponse): string {
  const secs = `${(Number(res.durationMs) / 1000).toFixed(1)}s`;
  const head = res.head ? ` · ${res.head.slice(0, 7)}` : "";
  if (res.cloned) return `cloned${head} · ${secs}`;
  if (res.commitsAdvanced > 0) {
    return `+${res.commitsAdvanced} commit${res.commitsAdvanced === 1 ? "" : "s"}${head} · ${secs}`;
  }
  if (res.headChanged) return `head moved${head} · ${secs}`;
  if (!res.head) return `fetched, extent unknown · ${secs}`;
  return `already current · ${secs}`;
}

// One repo's last sync outcome. Kept per-row rather than in the modal's single
// `error` line so a failed sync of one repo during "Sync all" doesn't erase
// the result of the three that worked.
type SyncState = { text: string; failed: boolean };

// Read-only since docs/adr/0074: a repo is in the fleet while
// argocd-ukubi-bot is a collaborator on it, and its overrides live in
// infra-bootstrap. The one action left is warming the clone cache.
function RepoRow({ repo, onSync, syncing, busy, sync }: {
  repo: Repo;
  onSync: (repo: Repo) => void;
  // This row is the one in flight (label only).
  syncing: boolean;
  // Some row is in flight — possibly another one, mid "Sync all". Disabling
  // only the in-flight row let a second click land, and its finally-clause
  // cleared the shared indicator, re-enabling "Sync all" for a second
  // concurrent loop over the same repos.
  busy: boolean;
  sync: SyncState | undefined;
}) {
  return (
    <div className="py-1.5 border-b border-base-content/5 last:border-0">
    <div className="flex items-center gap-2 flex-wrap">
      <span className="text-sm font-medium w-28 truncate flex-none">{repo.name}</span>
      <span className="text-xs text-dim flex-1 min-w-0 truncate" title={repo.url}>{repo.fullName || repo.url}</span>
      <span className="text-xs flex-none">{repo.baseBranch || "main"}</span>
      {repo.image && <span className="text-xs text-dim w-32 truncate flex-none" title={repo.image}>{repo.image}</span>}
      {repo.clusterAccess && <span className="badge badge-sm badge-warning flex-none" title="Sessions get the kubectl shim (docs/adr/0037)">cluster</span>}
      {/* Refreshes the provisioner's clone cache for this repo. Nothing else
          advances it between pod creations, so a new repo has no cache at all
          until its first session pays for a cold clone. */}
      <button
        type="button"
        onClick={() => onSync(repo)}
        disabled={busy}
        title="Fetch this repo into the fleet's shared clone cache"
        className="btn btn-sm flex-none"
      >
        {syncing ? "Syncing…" : "Sync"}
      </button>
    </div>
    {sync && (
      <p className={`text-xs mt-1 ${sync.failed ? "text-error" : "text-dim"}`}>{sync.text}</p>
    )}
    </div>
  );
}

export function ManageReposModal() {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState<string | null>(null);
  const [syncState, setSyncState] = useState<Record<string, SyncState>>({});

  function load() {
    return client
      .listRepos({})
      .then((res) => {
        setRepos(res.repos);
        setLoaded(true);
      })
      .catch((err: Error) => {
        setError(err.message);
        setLoaded(true);
      });
  }
  // "No repos configured yet." is a claim, so it waits for the first answer.
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    if (dialogOpen) load();
  }, [dialogOpen]);

  function open() {
    setError(null);
    setDialogOpen(true);
  }

  function close() {
    setDialogOpen(false);
  }

  // The error message is shown verbatim rather than reduced to "sync failed":
  // core forwards git's own stderr ("fatal: could not read Username",
  // "Repository not found"), which is the entire diagnostic available.
  async function syncOne(repo: Repo) {
    setSyncing(repo.name);
    try {
      const res = await client.syncRepo({ name: repo.name });
      setSyncState((prev) => ({ ...prev, [repo.name]: { text: syncLabel(res), failed: false } }));
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setSyncState((prev) => ({ ...prev, [repo.name]: { text: msg, failed: true } }));
    } finally {
      setSyncing(null);
    }
  }

  // Sequential on purpose: the provisioner serializes per-repo git work behind
  // its own mutex anyway, and one-at-a-time keeps each repo's result attached
  // to its own row for free.
  async function syncAll() {
    for (const repo of repos) {
      await syncOne(repo);
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={open}
        className="px-3 py-1.5 rounded-md border border-base-content/10 text-xs font-medium hover:bg-base-content/5"
      >
        repos
      </button>

      <Modal open={dialogOpen} onClose={close} boxClassName="max-w-3xl">
        <div className="flex items-center justify-between mb-3">
          <h3 className="font-semibold text-base">Repos</h3>
          <button
            type="button"
            onClick={syncAll}
            disabled={syncing !== null || repos.length === 0}
            title="Fetch every repo into the fleet's shared clone cache"
            className="btn btn-sm"
          >
            {syncing ? `Syncing ${syncing}…` : "Sync all"}
          </button>
        </div>

        <div className="flex flex-col">
          {repos.map((r) => (
            <RepoRow
              key={r.name}
              repo={r}
              onSync={syncOne}
              syncing={syncing === r.name}
              busy={syncing !== null}
              sync={syncState[r.name]}
            />
          ))}
          {repos.length === 0 && <p className="text-sm text-dim py-2">{loaded ? "No repos yet." : "Loading…"}</p>}
        </div>

        <p className="text-xs text-dim mt-3 pt-3 border-t border-base-content/10">
          To add a repo, invite <code>argocd-ukubi-bot</code> as a collaborator; it shows up within two minutes.
          Remove the bot to remove it. Name, base branch, image and cluster access are set in infra-bootstrap's{" "}
          <code>gitops/bootstrap/fleet-repo-overrides.yaml</code>.
        </p>

        <InlineError message={error} />

        <div className="modal-action">
          <button type="button" className="btn btn-sm" onClick={close}>
            Close
          </button>
        </div>
      </Modal>

    </>
  );
}
