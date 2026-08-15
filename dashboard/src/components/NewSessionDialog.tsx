import { useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import type { PromptSnippet, Repo } from "../gen/agentfleet/v1/dashboard_pb";

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
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A session created by a submit whose message then failed to send (capacity
  // rejection is real at MAX_LIVE_SESSIONS). Retrying reuses it rather than
  // leaving a trail of empty rows behind every failed attempt.
  const createdIdRef = useRef<string | null>(null);

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
    createdIdRef.current = null;
  }

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
      let sessionId = createdIdRef.current;
      if (!sessionId) {
        const res = await client.createSession({ repo, title: title.trim() || labelFrom(text) });
        sessionId = res.session?.id ?? null;
        if (!sessionId) throw new Error("CreateSession returned no session");
        createdIdRef.current = sessionId;
      }

      // Before postMessage, never after: postMessage is what provisions the
      // pod, and the worker reads session.permissionMode once at startup
      // (worker/src/session.ts:433). Setting it afterwards would reach a pod
      // that has already chosen its mode.
      const mode = snippets.find(
        (s) => snippetIds.includes(s.id) && s.suggestedPermissionMode,
      )?.suggestedPermissionMode;
      if (mode) await client.setPermissionMode({ sessionId, mode });

      // PostMessage warms first and appends second — resumeFromSeq is computed
      // from LatestSeq at provisioning time, so an entry written before the pod
      // exists lands below its cursor and is never delivered. That ordering
      // lives in core (core/internal/dashboard/server.go:492-500); do not
      // reproduce it here by calling warmSession first.
      if (text) await client.postMessage({ sessionId, text });

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
