import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";

export function NewTaskDialog({
  onCreated,
}: {
  onCreated: (taskId: string) => void;
}) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [repoNames, setRepoNames] = useState<string[]>([]);
  const [repo, setRepo] = useState("");
  const [description, setDescription] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Fetched from the dashboard-editable repos table (docs/adr/0028), not a
  // hardcoded list — a repo added/removed via ManageReposModal shows up
  // here without a redeploy.
  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listRepos({})
      .then((res) => {
        const names = res.repos.map((r) => r.name);
        setRepoNames(names);
        setRepo((current) => current || names[0] || "");
      })
      .catch((err: Error) => setError(err.message));
  }, [dialogOpen]);

  function open() {
    setError(null);
    setDialogOpen(true);
  }

  function close() {
    setDialogOpen(false);
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!description.trim()) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await client.createTask({ repo, description });
      setDescription("");
      close();
      if (res.task) onCreated(res.task.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <>
      <button
        type="button"
        onClick={open}
        className="px-3 py-1.5 rounded-md bg-base-content text-base-100 text-[11px] font-semibold hover:opacity-90"
      >
        + send a task
      </button>

      <Modal open={dialogOpen} onClose={close}>
        <h3 className="font-semibold text-base mb-3">New task</h3>
        <form onSubmit={handleSubmit} className="flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm">
            Repo
            <select
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              className="select select-bordered select-sm"
            >
              {repoNames.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
          <label className="flex flex-col gap-1 text-sm">
            Description
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="What should the agent do?"
              className="textarea textarea-bordered textarea-sm"
              rows={4}
              required
            />
          </label>
          {error && <p className="text-error text-sm">{error}</p>}
          <div className="modal-action">
            <button type="button" className="btn btn-sm" onClick={close}>
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting || !repo || !description.trim()}
              className="btn btn-sm btn-primary"
            >
              {submitting ? "Creating…" : "Create"}
            </button>
          </div>
        </form>
      </Modal>
    </>
  );
}
