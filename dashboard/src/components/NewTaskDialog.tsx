import { useRef, useState } from "react";
import { client } from "../connectClient";

// Mirrors core/internal/tasks/store.go's KnownRepos — every caller of
// CreateTask (bot/core/dashboard) keeps its own copy of this list already
// (see that file's own comment), so hardcoding here is consistent with
// existing precedent, not new debt.
const KNOWN_REPOS = ["dream-analyst", "vos-monolith"];

export function NewTaskDialog({
  onCreated,
}: {
  onCreated: (taskId: string) => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [repo, setRepo] = useState(KNOWN_REPOS[0]);
  const [description, setDescription] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function open() {
    setError(null);
    dialogRef.current?.showModal();
  }

  function close() {
    dialogRef.current?.close();
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

      <dialog ref={dialogRef} className="modal">
        <div className="modal-box">
          <h3 className="font-semibold text-base mb-3">New task</h3>
          <form onSubmit={handleSubmit} className="flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-sm">
              Repo
              <select
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                className="select select-bordered select-sm"
              >
                {KNOWN_REPOS.map((r) => (
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
                disabled={submitting || !description.trim()}
                className="btn btn-sm btn-primary"
              >
                {submitting ? "Creating…" : "Create"}
              </button>
            </div>
          </form>
        </div>
        <form method="dialog" className="modal-backdrop">
          <button>close</button>
        </form>
      </dialog>
    </>
  );
}
