import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import type { PromptSnippet } from "../gen/agentfleet/v1/dashboard_pb";

export function NewTaskDialog({
  onCreated,
}: {
  onCreated: (taskId: string) => void;
}) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [repoNames, setRepoNames] = useState<string[]>([]);
  const [repo, setRepo] = useState("");
  // docs/adr/0037: a cluster session is a normal task on infra-bootstrap
  // with kind=thot. Surfaced as its own checkbox rather than inferred
  // from the repo, because plain infra-bootstrap tasks are legitimate too
  // (editing manifests without wanting cluster access).
  const [clusterSession, setClusterSession] = useState(false);
  const [description, setDescription] = useState("");
  const [snippets, setSnippets] = useState<PromptSnippet[]>([]);
  const [snippetIds, setSnippetIds] = useState<string[]>([]);
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

  // Same dashboard-editable, no-redeploy pattern as repos above — a task
  // gets none of these by default (see ManagePromptSnippetsModal); this is
  // just the picker for optionally attaching some.
  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listPromptSnippets({})
      .then((res) => setSnippets(res.snippets))
      .catch((err: Error) => setError(err.message));
  }, [dialogOpen]);

  function toggleSnippet(id: string) {
    setSnippetIds((current) => (current.includes(id) ? current.filter((x) => x !== id) : [...current, id]));
  }

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
      const res = await client.createTask({ repo: clusterSession ? "infra-bootstrap" : repo, description, snippetIds, kind: clusterSession ? "thot" : "worker" });
      setDescription("");
      setSnippetIds([]);
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
          <label
            className="flex items-center gap-2 text-sm cursor-pointer"
            title="Runs on infra-bootstrap with cluster access. Reads answer without interrupting you; anything that changes the cluster asks first."
          >
            <input
              type="checkbox"
              checked={clusterSession}
              onChange={(e) => setClusterSession(e.target.checked)}
              className="checkbox checkbox-sm"
            />
            Cluster session (thot)
          </label>
          <label className={`flex flex-col gap-1 text-sm ${clusterSession ? "opacity-40" : ""}`}>
            Repo
            <select
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              disabled={clusterSession}
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
          {snippets.length > 0 && (
            <fieldset className="flex flex-col gap-1 text-sm">
              <legend className="mb-1">Guidance (optional)</legend>
              {snippets.map((s) => (
                <label key={s.id} className="flex items-center gap-2 text-[12px]">
                  <input
                    type="checkbox"
                    checked={snippetIds.includes(s.id)}
                    onChange={() => toggleSnippet(s.id)}
                    className="checkbox checkbox-sm"
                  />
                  {s.name}
                </label>
              ))}
            </fieldset>
          )}
          {error && <p className="text-error text-sm">{error}</p>}
          <div className="modal-action">
            <button type="button" className="btn btn-sm" onClick={close}>
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting || (!clusterSession && !repo) || !description.trim()}
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
