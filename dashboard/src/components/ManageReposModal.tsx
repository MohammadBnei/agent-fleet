import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import type { Repo } from "../gen/agentfleet/v1/dashboard_pb";

// Dashboard-editable target-repo config (docs/adr/0028) — replaces the old
// hardcoded KNOWN_REPOS array here and the equally-hardcoded
// core/internal/tasks.KnownRepos Go map, so onboarding/editing a repo no
// longer needs a core redeploy.

function RepoRow({ repo, onSaved, onRequestDelete, onError }: {
  repo: Repo;
  onSaved: () => void;
  onRequestDelete: (repo: Repo) => void;
  onError: (msg: string) => void;
}) {
  const [url, setUrl] = useState(repo.url);
  const [baseBranch, setBaseBranch] = useState(repo.baseBranch);
  const [saving, setSaving] = useState(false);
  const dirty = url !== repo.url || baseBranch !== repo.baseBranch;

  async function save() {
    setSaving(true);
    try {
      await client.updateRepo({ name: repo.name, url, baseBranch });
      onSaved();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex items-center gap-2 py-1.5 border-b border-base-content/5 last:border-0">
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
      <button
        type="button"
        onClick={save}
        disabled={!dirty || saving}
        className="btn btn-sm flex-none"
      >
        Save
      </button>
      <button type="button" onClick={() => onRequestDelete(repo)} className="btn btn-sm btn-error flex-none">
        Delete
      </button>
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
  const [creating, setCreating] = useState(false);
  // Rendered as a sibling of <Modal> below, never nested inside it — a
  // <dialog> nested inside another open <dialog> closes both together on
  // Cancel in this browser (confirmed empirically), not just the inner one.
  const [pendingDelete, setPendingDelete] = useState<Repo | null>(null);

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

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !url.trim()) return;
    setCreating(true);
    setError(null);
    try {
      await client.createRepo({ name, url, baseBranch });
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
        className="px-3 py-1.5 rounded-md border border-base-content/10 text-[11px] font-medium hover:bg-base-content/5"
      >
        manage repos
      </button>

      <Modal open={dialogOpen} onClose={close} boxClassName="max-w-lg">
        <h3 className="font-semibold text-base mb-3">Manage repos</h3>

        <div className="flex flex-col">
          {repos.map((r) => (
            <RepoRow key={r.name} repo={r} onSaved={refresh} onRequestDelete={setPendingDelete} onError={setError} />
          ))}
          {repos.length === 0 && <p className="text-sm text-base-content/50 py-2">No repos configured yet.</p>}
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
            ? `Delete repo "${pendingDelete.name}"? Existing tasks keep their history; new tasks can no longer target it.`
            : ""
        }
        confirmWord="delete"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
