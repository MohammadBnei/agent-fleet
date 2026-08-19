import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { queueFirstMessage } from "../useSessionDetail";
import { Modal } from "./Modal";
import type { PromptSnippet, Repo } from "../gen/agentfleet/v1/dashboard_pb";
import { AUTO_MODE_WARNING, PERMISSION_MODES } from "../approvePlan";

// The first line of the message, as a list label. `description`/`title` are
// human-facing labels the agent never reads (db/migrations/000001_init.up.sql
// :66-71), so this is a copy for the list — never the prompt itself, which
// goes out as a real transcript entry below.
function labelFrom(message: string): string {
  const line = message.trim().split("\n", 1)[0]?.trim() ?? "";
  return line.length > 80 ? `${line.slice(0, 79)}…` : line;
}

export function NewSessionDialog({
  onCreated,
  // Mobile's top bar has room for a glyph, not a label — the console mobile
  // mockup shows this as a bare "+".
  compact = false,
  // The manifest's "New session" app shortcut lands on /?new=1 and expects the
  // form to be open on arrival; a shortcut that just shows the list would be a
  // dead affordance.
  autoOpen = false,
}: {
  onCreated: (sessionId: string) => void;
  compact?: boolean;
  autoOpen?: boolean;
}) {
  const [dialogOpen, setDialogOpen] = useState(autoOpen);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [repo, setRepo] = useState("");
  const [title, setTitle] = useState("");
  const [message, setMessage] = useState("");
  const [snippets, setSnippets] = useState<PromptSnippet[]>([]);
  const [snippetIds, setSnippetIds] = useState<string[]>([]);
  // null = "follow whatever the selected snippets suggest". A real value means
  // the human picked one, and an explicit pick outranks a suggestion.
  const [modeChoice, setModeChoice] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // createdIdRef is gone with the awaited postMessage it existed for: a submit
  // that failed mid-send used to leave a created session behind, so a retry
  // had to reuse it. Now the only awaited call is CreateSession itself — if
  // that fails there is no row, and if it succeeds the dialog is already
  // closed. A failed first message surfaces in the session it belongs to.

  // Fetched from the dashboard-editable repos table (docs/adr/0028), not a
  // hardcoded list — a repo added/removed via ManageReposModal shows up
  // here without a redeploy. Kept whole rather than reduced to names: the
  // option label marks the ones carrying cluster access.
  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listRepos({})
      .then((res) => {
        setRepos(res.repos);
        setRepo((current) => current || res.repos[0]?.name || "");
      })
      .catch((err: Error) => setError(err.message));
  }, [dialogOpen]);

  // Same dashboard-editable, no-redeploy pattern as repos above. Snippets are
  // no longer sent to the server — `snippet_ids` is a reserved field
  // (proto/agentfleet/v1/dashboard.proto:42-49), because resolving them into
  // a hidden `guidance` blob is exactly what docs/adr/0048 deleted. They
  // prefill the message instead, where the human can see and edit them.
  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listPromptSnippets({})
      .then((res) => setSnippets(res.snippets))
      .catch((err: Error) => setError(err.message));
  }, [dialogOpen]);

  // ponytail: the textarea is the source of truth, not the checkboxes. Ticking
  // inserts the snippet's text; unticking removes it only if it is still there
  // verbatim, so text the human edited first survives. The checkbox is an
  // inserter, not a two-way binding — which is the point, since what gets sent
  // is whatever is in the box.
  function toggleSnippet(s: PromptSnippet) {
    const on = snippetIds.includes(s.id);
    setSnippetIds((current) => (on ? current.filter((x) => x !== s.id) : [...current, s.id]));
    setMessage((current) =>
      on
        ? current.replace(`${s.text}\n\n`, "").replace(s.text, "")
        : `${s.text}\n\n${current}`,
    );
  }

  function open() {
    setError(null);
    setDialogOpen(true);
  }

  // Closing always resets, including after a failed send: the session that was
  // created still exists, and an empty session is a valid resting state, so
  // the next open starts a new one rather than silently adopting the last.
  function close() {
    setDialogOpen(false);
    reset();
  }

  function reset() {
    setTitle("");
    setMessage("");
    setSnippetIds([]);
    setModeChoice(null);
  }

  // A snippet may suggest a mode (prompt_snippets.suggested_permission_mode).
  // It PRE-SELECTS the picker rather than being applied invisibly behind it:
  // the human sees the mode they are about to launch in, and can override it.
  // An explicit pick always wins over a suggestion.
  const suggestedMode = snippets.find(
    (s) => snippetIds.includes(s.id) && s.suggestedPermissionMode,
  )?.suggestedPermissionMode;
  const effectiveMode = modeChoice ?? suggestedMode ?? "default";

  // docs/adr/0048 §1: CreateSession writes a row and nothing else, and the
  // FIRST MESSAGE is what boots the pod. Sending an instruction as
  // `description` — which is what this dialog used to do — put it in a column
  // the agent never reads and left the session sitting idle with no pod and no
  // error anywhere.
  //
  // A session with no message is a valid resting state, so the message is
  // optional: "create empty, then talk to it" is a supported flow, and it is
  // the human gate expressed structurally (nothing machine-initiated produces
  // a message, so nothing machine-initiated produces a pod).
  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!repo) return;
    setSubmitting(true);
    setError(null);
    const text = message.trim();
    try {
      const res = await client.createSession({
        repo,
        title: title.trim() || labelFrom(text),
        // Carried on CreateSession itself rather than a setPermissionMode call
        // afterwards. The worker reads session.permissionMode once at startup,
        // and the first message is what provisions the pod, so the old
        // two-call shape only worked because it happened to run in the right
        // order. One call cannot be ordered wrongly.
        permissionMode: effectiveMode,
      });
      const sessionId = res.session?.id;
      if (!sessionId) throw new Error("CreateSession returned no session");

      // Handed to the detail view instead of awaited here. PostMessage IS the
      // pod boot — clone, fleet-shared rsync onto a ~10MB/s RWX volume, PVC,
      // Job — so awaiting it held this modal open for the whole of it while
      // the session row had existed since the line above. The detail view
      // renders the provisioning progress that nobody could see behind the
      // modal ("PROVISIONING · cloning repo"), plus the message itself with a
      // spinner, and surfaces a failure inline where the session is.
      //
      // The ordering that matters is untouched: PostMessage still warms before
      // it appends, server-side (dashboard.Server.WarmIfIdle).
      if (text) queueFirstMessage(sessionId, text);

      close();
      onCreated(sessionId);
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
        aria-label="New session"
        className={
          compact
            ? "text-lg text-dim hover:text-primary px-1 flex-none"
            : "flex-none border border-acc-line px-3 py-1.5 text-xs hover:border-primary hover:text-primary"
        }
      >
        {compact ? "+" : "+ new session"}
      </button>

      <Modal open={dialogOpen} onClose={close}>
        <h3 className="font-semibold text-base mb-3">New session</h3>
        <form onSubmit={handleSubmit} className="flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm">
            Repo
            <select
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              className="select select-bordered select-sm"
            >
              {repos.map((r) => (
                <option key={r.name} value={r.name}>
                  {r.clusterAccess ? `${r.name} · cluster access` : r.name}
                </option>
              ))}
            </select>
          </label>
          <label className="flex flex-col gap-1 text-sm">
            Title (optional)
            <input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="a label for the list — defaults to the first line below"
              className="input input-bordered input-sm"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            First message (optional)
            <textarea
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              placeholder="What should the agent do? Leave empty to create the session without starting it."
              className="textarea textarea-bordered textarea-sm"
              rows={6}
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span>Launch mode</span>
            <select
              value={effectiveMode}
              onChange={(e) => setModeChoice(e.target.value)}
              className="select select-sm select-bordered"
            >
              {PERMISSION_MODES.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
            {/* Stated here, not linked from here. This is the only moment the
                human chooses this mode, and the actions menu's own confirm
                (SessionActionsModal) never runs for a session that launches in
                auto — so if the consequence is not on screen at this point, it
                is nowhere. */}
            {effectiveMode === "auto" && <span className="text-warning text-xs">{AUTO_MODE_WARNING}</span>}
            {suggestedMode && modeChoice === null && (
              <span className="text-dim text-xs">Suggested by the guidance you selected — change it if you want.</span>
            )}
          </label>
          {snippets.length > 0 && (
            <fieldset className="flex flex-col gap-1 text-sm">
              <legend className="mb-1">Guidance (optional)</legend>
              {snippets.map((s) => (
                <label key={s.id} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={snippetIds.includes(s.id)}
                    onChange={() => toggleSnippet(s)}
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
            <button type="submit" disabled={submitting || !repo} className="btn btn-sm btn-primary">
              {submitting ? "Creating…" : message.trim() ? "Create & send" : "Create"}
            </button>
          </div>
        </form>
      </Modal>
    </>
  );
}
