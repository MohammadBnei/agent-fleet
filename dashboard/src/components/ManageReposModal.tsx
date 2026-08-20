import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import type { Repo, SyncRepoResponse } from "../gen/agentfleet/v1/dashboard_pb";

// Dashboard-editable target-repo config (docs/adr/0028) — replaces the old
// hardcoded KNOWN_REPOS array here and the equally-hardcoded
// core/internal/sessions.KnownRepos Go map, so onboarding/editing a repo no
// longer needs a core redeploy.

// What a finished sync says. The point of the button is answering "did that
// do anything?", so a bare "done" would waste the whole round trip — the
// provisioner already counted the commits and timed itself.
function syncLabel(res: SyncRepoResponse): string {
  const secs = `${(Number(res.durationMs) / 1000).toFixed(1)}s`;
  if (res.cloned) return `cloned · ${secs}`;
  if (res.commitsAdvanced > 0) {
    const head = res.head ? ` · ${res.head.slice(0, 7)}` : "";
    return `+${res.commitsAdvanced} commit${res.commitsAdvanced === 1 ? "" : "s"}${head} · ${secs}`;
  }
  return `already current · ${secs}`;
}

// One repo's last sync outcome. Kept per-row rather than in the modal's single
// `error` line so a failed sync of one repo during "Sync all" doesn't erase
// the result of the three that worked.
type SyncState = { text: string; failed: boolean };

function RepoRow({ repo, onSaved, onRequestDelete, onError, onSync, syncing, sync }: {
  repo: Repo;
  onSaved: () => void;
  onRequestDelete: (repo: Repo) => void;
  onError: (msg: string) => void;
  onSync: (repo: Repo) => void;
  syncing: boolean;
  sync: SyncState | undefined;
}) {
  const [url, setUrl] = useState(repo.url);
  const [baseBranch, setBaseBranch] = useState(repo.baseBranch);
  const [image, setImage] = useState(repo.image);
  const [saving, setSaving] = useState(false);
  const dirty =
    url !== repo.url || baseBranch !== repo.baseBranch || image !== repo.image;

  async function save() {
    setSaving(true);
    try {
      await client.updateRepo({ name: repo.name, url, baseBranch, image, clusterAccess: repo.clusterAccess });
      onSaved();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="py-1.5 border-b border-base-content/5 last:border-0">
    <div className="flex items-center gap-2">
      <span className="text-sm font-medium w-28 truncate flex-none">{repo.name}</span>
      <input
        value={url}
        onChange={(e) => setUrl(e.target.value)}
        className="input input-sm input-bordered flex-1 min-w-0"
      />
      <input
        value={baseBranch}
        onChange={(e) => setBaseBranch(e.target.value)}
        placeholder="main"
        className="input input-sm input-bordered w-24 flex-none"
      />
      {/* The container image this repo's sessions run in (repos.image,
          docs/adr/0048 §6 — one column replacing three profile tables).
          Blank means the fleet's default worker image, which carries bun, Go,
          git, gh and a browser.

          This input was still labelled as the e2e sandbox's build profile
          while already writing `image`, so the UI described a field that no
          longer existed and hid the one it was actually editing. */}
      <input
        value={image}
        onChange={(e) => setImage(e.target.value)}
        placeholder="default image"
        title="Container image this repo's sessions run in — blank uses the fleet's default worker image"
        className="input input-sm input-bordered w-32 flex-none"
      />
      <button
        type="button"
        onClick={save}
        disabled={!dirty || saving}
        className="btn btn-sm flex-none"
      >
        Save
      </button>
      {/* Refreshes the provisioner's clone cache for this repo. Nothing else
          advances it between pod creations, so a repo added here has no cache
          at all until its first session pays for a cold clone. */}
      <button
        type="button"
        onClick={() => onSync(repo)}
        disabled={syncing}
        title="Fetch this repo into the fleet's shared clone cache"
        className="btn btn-sm flex-none"
      >
        {syncing ? "Syncing…" : "Sync"}
      </button>
      <button type="button" onClick={() => onRequestDelete(repo)} className="btn btn-sm btn-error flex-none">
        Delete
      </button>
    </div>
    {sync && (
      <p className={`text-xs mt-1 ${sync.failed ? "text-error" : "text-dim"}`}>{sync.text}</p>
    )}
    </div>
  );
}

export function ManageReposModal({ onChanged }: { onChanged?: () => void }) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [baseBranch, setBaseBranch] = useState("");
  const [image, setImage] = useState("");
  const [creating, setCreating] = useState(false);
  // Rendered as a sibling of <Modal> below, never nested inside it — a
  // <dialog> nested inside another open <dialog> closes both together on
  // Cancel in this browser (confirmed empirically), not just the inner one.
  const [pendingDelete, setPendingDelete] = useState<Repo | null>(null);
  const [syncing, setSyncing] = useState<string | null>(null);
  const [syncState, setSyncState] = useState<Record<string, SyncState>>({});

  function load() {
    return client
      .listRepos({})
      .then((res) => setRepos(res.repos))
      .catch((err: Error) => setError(err.message));
  }

  useEffect(() => {
    if (dialogOpen) load();
  }, [dialogOpen]);

  function refresh() {
    load();
    onChanged?.();
  }

  function open() {
    setError(null);
    setDialogOpen(true);
  }

  function close() {
    setDialogOpen(false);
  }

  async function confirmDelete() {
    const repo = pendingDelete;
    setPendingDelete(null);
    if (!repo) return;
    try {
      await client.deleteRepo({ name: repo.name });
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
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

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !url.trim()) return;
    setCreating(true);
    setError(null);
    try {
      await client.createRepo({ name, url, baseBranch, image, clusterAccess: false });
      setName("");
      setUrl("");
      setBaseBranch("");
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={open}
        className="px-3 py-1.5 rounded-md border border-base-content/10 text-xs font-medium hover:bg-base-content/5"
      >
        manage repos
      </button>

      <Modal open={dialogOpen} onClose={close} boxClassName="max-w-lg">
        <div className="flex items-center justify-between mb-3">
          <h3 className="font-semibold text-base">Manage repos</h3>
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
              onSaved={refresh}
              onRequestDelete={setPendingDelete}
              onError={setError}
              onSync={syncOne}
              syncing={syncing === r.name}
              sync={syncState[r.name]}
            />
          ))}
          {repos.length === 0 && <p className="text-sm text-dim py-2">No repos configured yet.</p>}
        </div>

        <form onSubmit={handleCreate} className="flex items-center gap-2 mt-3 pt-3 border-t border-base-content/10">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="name"
            className="input input-sm input-bordered w-28 flex-none"
            required
          />
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="git URL"
            className="input input-sm input-bordered flex-1 min-w-0"
            required
          />
          <input
            value={baseBranch}
            onChange={(e) => setBaseBranch(e.target.value)}
            placeholder="main"
            className="input input-sm input-bordered w-24 flex-none"
          />
          <input
            value={image}
            onChange={(e) => setImage(e.target.value)}
            placeholder="default image"
            title="Container image this repo's sessions run in — blank uses the fleet's default worker image"
            className="input input-sm input-bordered w-32 flex-none"
          />
          <button type="submit" disabled={creating || !name.trim() || !url.trim()} className="btn btn-sm btn-primary flex-none">
            {creating ? "Adding…" : "Add"}
          </button>
        </form>

        {error && <p className="text-error text-sm mt-2">{error}</p>}

        <div className="modal-action">
          <button type="button" className="btn btn-sm" onClick={close}>
            Close
          </button>
        </div>
      </Modal>

      <ConfirmModal
        open={pendingDelete !== null}
        message={
          pendingDelete
            ? `Delete repo "${pendingDelete.name}"? Existing sessions keep their history; new sessions can no longer target it.`
            : ""
        }
        confirmWord="delete"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
