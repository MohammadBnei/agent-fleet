import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import type { PromptSnippet } from "../gen/agentfleet/v1/dashboard_pb";

// Dashboard-editable, reusable guidance text an operator optionally
// attaches to a task at creation time (see NewTaskDialog's snippet
// checkboxes) — replaces worker/src/session.ts's old unconditional,
// hardcoded taskPrompt() workflow. Structurally a clone of
// ManageReposModal.tsx, same no-redeploy CRUD pattern.

function SnippetRow({ snippet, onSaved, onRequestDelete, onError }: {
  snippet: PromptSnippet;
  onSaved: () => void;
  onRequestDelete: (snippet: PromptSnippet) => void;
  onError: (msg: string) => void;
}) {
  const [name, setName] = useState(snippet.name);
  const [text, setText] = useState(snippet.text);
  const [saving, setSaving] = useState(false);
  const dirty = name !== snippet.name || text !== snippet.text;

  async function save() {
    setSaving(true);
    try {
      await client.updatePromptSnippet({ id: snippet.id, name, text });
      onSaved();
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-1.5 py-2 border-b border-base-content/5 last:border-0">
      <div className="flex items-center gap-2">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="input input-sm input-bordered flex-1 min-w-0 font-medium"
        />
        <button type="button" onClick={save} disabled={!dirty || saving} className="btn btn-sm flex-none">
          Save
        </button>
        <button type="button" onClick={() => onRequestDelete(snippet)} className="btn btn-sm btn-error flex-none">
          Delete
        </button>
      </div>
      <textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        rows={3}
        className="textarea textarea-bordered textarea-sm w-full"
      />
    </div>
  );
}

export function ManagePromptSnippetsModal({ onChanged }: { onChanged?: () => void }) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [snippets, setSnippets] = useState<PromptSnippet[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [text, setText] = useState("");
  const [creating, setCreating] = useState(false);
  // Rendered as a sibling of <Modal> below, never nested inside it — a
  // <dialog> nested inside another open <dialog> closes both together on
  // Cancel in this browser (confirmed empirically for ManageReposModal),
  // not just the inner one.
  const [pendingDelete, setPendingDelete] = useState<PromptSnippet | null>(null);

  function load() {
    return client
      .listPromptSnippets({})
      .then((res) => setSnippets(res.snippets))
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
    const snippet = pendingDelete;
    setPendingDelete(null);
    if (!snippet) return;
    try {
      await client.deletePromptSnippet({ id: snippet.id });
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim() || !text.trim()) return;
    setCreating(true);
    setError(null);
    try {
      await client.createPromptSnippet({ name, text });
      setName("");
      setText("");
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
        manage guidance
      </button>

      <Modal open={dialogOpen} onClose={close} boxClassName="max-w-lg">
        <h3 className="font-semibold text-base mb-3">Manage guidance</h3>
        <p className="text-[11.5px] text-base-content/50 mb-3">
          Optional, reusable instructions an operator can attach to a task at creation time — a task with none
          attached gets just its own description, nothing more.
        </p>

        <div className="flex flex-col">
          {snippets.map((s) => (
            <SnippetRow key={s.id} snippet={s} onSaved={refresh} onRequestDelete={setPendingDelete} onError={setError} />
          ))}
          {snippets.length === 0 && <p className="text-sm text-base-content/50 py-2">No guidance snippets yet.</p>}
        </div>

        <form onSubmit={handleCreate} className="flex flex-col gap-2 mt-3 pt-3 border-t border-base-content/10">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="name"
            className="input input-sm input-bordered w-full"
            required
          />
          <textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="guidance text"
            rows={3}
            className="textarea textarea-bordered textarea-sm w-full"
            required
          />
          <button
            type="submit"
            disabled={creating || !name.trim() || !text.trim()}
            className="btn btn-sm btn-primary self-end"
          >
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
        message={pendingDelete ? `Delete guidance snippet "${pendingDelete.name}"? Tasks already using it keep their frozen copy.` : ""}
        confirmWord="delete"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
