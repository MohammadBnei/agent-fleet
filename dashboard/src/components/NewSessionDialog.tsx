import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { activeRepos } from "../repoFilter";
import { queueFirstMessage } from "../useSessionDetail";
import { Modal } from "./Modal";
import type { AgentDefinition, Repo } from "../gen/agentfleet/v1/dashboard_pb";
import { AUTO_MODE_WARNING, PERMISSION_MODES } from "../approvePlan";
import { InlineError } from "./InlineError";

// The first line of the message, as a list label. `description`/`title` are
// human-facing labels the agent never reads (db/migrations/000001_init.up.sql
// :66-71), so this is a copy for the list — never the prompt itself, which
// goes out as a real transcript entry below.
// The label for "pass nothing": core stores NULL, the worker omits the option,
// and Claude Code picks (docs/adr/0077).
const CLAUDE_DEFAULT = "Claude's default";
// The SDK's EffortLevel; core validates against the same list.
const EFFORTS = ["low", "medium", "high", "xhigh", "max"];

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
  // From ListRepos, so core's allowlist is the only list. "" = Claude's default.
  const [models, setModels] = useState<string[]>([]);
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  // docs/adr/0062: a defined agent fixes repo, model and mode itself, so
  // choosing one hides those controls rather than letting them disagree.
  const [definitions, setDefinitions] = useState<AgentDefinition[]>([]);
  const [syncErrors, setSyncErrors] = useState<string[]>([]);
  const [definition, setDefinition] = useState("");
  const [title, setTitle] = useState("");
  const [message, setMessage] = useState("");
  // null = the launch default; a real value is the human's pick.
  const [modeChoice, setModeChoice] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // createdIdRef is gone with the awaited postMessage it existed for: a submit
  // that failed mid-send used to leave a created session behind, so a retry
  // had to reuse it. Now the only awaited call is CreateSession itself — if
  // that fails there is no row, and if it succeeds the dialog is already
  // closed. A failed first message surfaces in the session it belongs to.

  // Fetched from the repos table (docs/adr/0028), not a hardcoded list — a
  // repo the GitHub sync adds or removes (docs/adr/0074) shows up here
  // without a redeploy. Kept whole rather than reduced to names: the
  // option label marks the ones carrying cluster access.
  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listRepos({})
      .then((res) => {
        // Inactive repos start no session; core would refuse them anyway. A
        // remembered pick that has since been switched off falls back too.
        const active = activeRepos(res.repos);
        setRepos(active);
        setModels(res.models);
        setRepo((current) => (active.some((r) => r.name === current) ? current : active[0]?.name || ""));
      })
      .catch((err: Error) => setError(err.message));
  }, [dialogOpen]);

  useEffect(() => {
    if (!dialogOpen) return;
    client
      .listDefinitions({})
      .then((res) => {
        setDefinitions(res.definitions);
        setSyncErrors(res.syncErrors);
      })
      // Under the picker, not the dialog's error banner: a definitions
      // outage must not mark repo-session creation as failing.
      .catch((err: Error) => setSyncErrors([`definitions unavailable: ${err.message}`]));
  }, [dialogOpen]);
  const chosen = definitions.find((d) => d.name === definition);

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
    setModeChoice(null);
    setDefinition("");
    setModel("");
    setEffort("");
  }

  // Without an explicit pick, "auto" — the fleet's launch default since
  // docs/adr/0061, same as core's column default.
  const effectiveMode = modeChoice ?? "auto";

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
    if (!repo && !definition) return;
    setSubmitting(true);
    setError(null);
    const text = message.trim();
    try {
      const res = await client.createSession(
        definition
          ? // Repo, model and mode come from the definition; core refuses them here.
            { definition, title: title.trim() || labelFrom(text) }
          : {
              repo,
              title: title.trim() || labelFrom(text),
              // Carried on CreateSession itself rather than a setPermissionMode call
              // afterwards. The worker reads session.permissionMode once at startup,
              // and the first message is what provisions the pod, so the old
              // two-call shape only worked because it happened to run in the right
              // order. One call cannot be ordered wrongly.
              permissionMode: effectiveMode,
              // Omitted when unpicked, so core stores NULL and Claude Code decides.
              ...(model ? { model } : {}),
              ...(effort ? { effort } : {}),
            },
      );
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
          {(definitions.length > 0 || syncErrors.length > 0) && (
            <label className="flex flex-col gap-1 text-sm">
              Definition
              <select
                value={definition}
                onChange={(e) => setDefinition(e.target.value)}
                className="select select-bordered select-sm"
              >
                <option value="">— none (repo session) —</option>
                {definitions.map((d) => (
                  <option
                    key={d.name}
                    value={d.name}
                    disabled={!d.valid}
                    title={d.valid ? undefined : d.error}
                  >
                    {d.valid
                      ? `${d.name} · ${d.repo} · ${d.model || CLAUDE_DEFAULT} · ${d.permissionMode}`
                      : `${d.name} · invalid`}
                  </option>
                ))}
              </select>
              {chosen && (
                <span className="text-dim text-xs">
                  Runs {chosen.repo} as {chosen.permissionMode} on {chosen.model || CLAUDE_DEFAULT} — {chosen.name}@{chosen.sha.slice(0, 7)}
                </span>
              )}
              {syncErrors.length > 0 && (
                <ul className="text-dim2 text-xs list-disc pl-4">
                  {syncErrors.map((e) => (
                    <li key={e}>{e}</li>
                  ))}
                </ul>
              )}
            </label>
          )}
          {!definition && (
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
          )}
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
          {/* Repo sessions only: a definition carries its own model and
              thinking (docs/adr/0062), and core refuses both here. */}
          {!definition && (
            <div className="flex gap-3">
              <label className="flex flex-col gap-1 text-sm flex-1 min-w-0">
                Model
                <select value={model} onChange={(e) => setModel(e.target.value)} className="select select-sm select-bordered">
                  <option value="">{CLAUDE_DEFAULT}</option>
                  {models.map((m) => (
                    <option key={m} value={m}>
                      {m}
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex flex-col gap-1 text-sm flex-1 min-w-0">
                Effort
                <select value={effort} onChange={(e) => setEffort(e.target.value)} className="select select-sm select-bordered">
                  <option value="">{CLAUDE_DEFAULT}</option>
                  {EFFORTS.map((e) => (
                    <option key={e} value={e}>
                      {e}
                    </option>
                  ))}
                </select>
              </label>
            </div>
          )}
          {!definition && (
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
              {/* Stated here, not linked from here. Auto is the pre-selected
                  default (docs/adr/0061), so this is the only place the human
                  sees what that means — the actions menu's own confirm
                  (SessionActionsModal) never runs for a session that launches in
                  auto. If the consequence is not on screen here, it is nowhere. */}
              {effectiveMode === "auto" && <span className="text-warning text-xs">{AUTO_MODE_WARNING}</span>}
            </label>
          )}
          <InlineError message={error} />
          <div className="modal-action">
            <button type="button" className="btn btn-sm" onClick={close}>
              Cancel
            </button>
            <button type="submit" disabled={submitting || (!repo && !definition)} className="btn btn-sm btn-primary">
              {submitting ? "Creating…" : message.trim() ? "Create & send" : "Create"}
            </button>
          </div>
        </form>
      </Modal>
    </>
  );
}
