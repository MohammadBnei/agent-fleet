import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { Modal } from "./Modal";
import { ConfirmModal } from "./ConfirmModal";
import type { RepoProfile } from "../gen/agentfleet/v1/dashboard_pb";
import { ScopeMode, type ServiceIngredient } from "../gen/agentfleet/v1/provisioner_pb";

// Dashboard-editable environment recipes (docs/adr/0034) — replaces the
// hardcoded per-repo StartCmdFor switch in provisioner/internal/k8s/names.go.
// A repo declares named profiles ("worker", "e2e", "lint", ...) built from a
// bounded ingredient catalog. These two lists must be kept in sync BY HAND
// with provisioner/internal/catalog/catalog.go — extending the catalog is a
// deliberate two-sided code change, not config-only (the provisioner
// rejects an unknown key at pod-materialization time regardless of what
// this UI lets you pick).
const KNOWN_TOOLS = ["go-toolchain", "bun-toolchain", "golangci-lint", "buf"] as const;
const KNOWN_SERVICES = ["postgres", "redis"] as const;

type ServiceState = Record<string, ScopeMode | null>; // null = not included

function servicesToState(services: ServiceIngredient[]): ServiceState {
  const state: ServiceState = {};
  for (const key of KNOWN_SERVICES) state[key] = null;
  for (const si of services) state[si.key] = si.scopeMode;
  return state;
}

// Loose init shape, not the full ServiceIngredient message type (which
// requires $typeName) — matches how every other RPC call in this codebase
// passes plain object literals (e.g. ManageReposModal's createRepo call),
// relying on connect-es's own MessageInitShape acceptance.
function stateToServices(state: ServiceState) {
  return KNOWN_SERVICES.filter((key) => state[key] != null).map((key) => ({
    key,
    scopeMode: state[key]!,
  }));
}

function sameTools(a: string[], b: string[]) {
  return a.length === b.length && a.every((t) => b.includes(t));
}

function sameServices(a: ServiceState, b: ServiceState) {
  return KNOWN_SERVICES.every((key) => a[key] === b[key]);
}

function ToolAndServiceEditor({
  tools,
  setTools,
  services,
  setServices,
}: {
  tools: string[];
  setTools: (t: string[]) => void;
  services: ServiceState;
  setServices: (s: ServiceState) => void;
}) {
  return (
    <div className="flex flex-wrap gap-3 text-xs">
      {KNOWN_TOOLS.map((tool) => (
        <label key={tool} className="flex items-center gap-1 cursor-pointer">
          <input
            type="checkbox"
            className="checkbox checkbox-xs"
            checked={tools.includes(tool)}
            onChange={(e) =>
              setTools(e.target.checked ? [...tools, tool] : tools.filter((t) => t !== tool))
            }
          />
          {tool}
        </label>
      ))}
      {KNOWN_SERVICES.map((svc) => (
        <div key={svc} className="flex items-center gap-1">
          <label className="flex items-center gap-1 cursor-pointer">
            <input
              type="checkbox"
              className="checkbox checkbox-xs"
              checked={services[svc] != null}
              onChange={(e) =>
                setServices({ ...services, [svc]: e.target.checked ? ScopeMode.TASK_SCOPED : null })
              }
            />
            {svc}
          </label>
          {services[svc] != null && (
            <select
              value={services[svc]!}
              onChange={(e) => setServices({ ...services, [svc]: Number(e.target.value) as ScopeMode })}
              className="select select-xs select-bordered"
            >
              <option value={ScopeMode.POD_SCOPED}>pod-scoped</option>
              <option value={ScopeMode.TASK_SCOPED}>task-scoped</option>
              <option value={ScopeMode.REPO_SCOPED}>repo-scoped</option>
            </select>
          )}
        </div>
      ))}
    </div>
  );
}

function ProfileRow({
  profile,
  onSaved,
  onRequestDelete,
  onError,
}: {
  profile: RepoProfile;
  onSaved: () => void;
  onRequestDelete: (profile: RepoProfile) => void;
  onError: (msg: string) => void;
}) {
  const [startCmd, setStartCmd] = useState(profile.startCmd);
  const [tools, setTools] = useState<string[]>(profile.toolKeys);
  const [services, setServices] = useState<ServiceState>(servicesToState(profile.serviceIngredients));
  const [saving, setSaving] = useState(false);
  const dirty =
    startCmd !== profile.startCmd ||
    !sameTools(tools, profile.toolKeys) ||
    !sameServices(services, servicesToState(profile.serviceIngredients));

  async function save() {
    setSaving(true);
    try {
      await client.updateRepoProfile({
        repoName: profile.repoName,
        name: profile.name,
        startCmd,
        toolKeys: tools,
        serviceIngredients: stateToServices(services),
      });
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
        <span className="text-sm font-medium w-20 truncate flex-none">{profile.name}</span>
        <input
          value={startCmd}
          onChange={(e) => setStartCmd(e.target.value)}
          placeholder="start command"
          className="input input-sm input-bordered flex-1 min-w-0 font-mono text-xs"
        />
        <button type="button" onClick={save} disabled={!dirty || saving} className="btn btn-sm flex-none">
          Save
        </button>
        <button type="button" onClick={() => onRequestDelete(profile)} className="btn btn-sm btn-error flex-none">
          Delete
        </button>
      </div>
      <ToolAndServiceEditor tools={tools} setTools={setTools} services={services} setServices={setServices} />
    </div>
  );
}

export function ManageRepoProfilesModal({ repoName }: { repoName: string }) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [profiles, setProfiles] = useState<RepoProfile[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [startCmd, setStartCmd] = useState("");
  const [tools, setTools] = useState<string[]>([]);
  const [services, setServices] = useState<ServiceState>(servicesToState([]));
  const [creating, setCreating] = useState(false);
  // Sibling of <Modal>, not nested — see ManageReposModal's identical
  // comment for the closes-both-together nested-<dialog> bug this avoids.
  const [pendingDelete, setPendingDelete] = useState<RepoProfile | null>(null);

  function load() {
    return client
      .listRepoProfiles({ repoName })
      .then((res) => setProfiles(res.profiles))
      .catch((err: Error) => setError(err.message));
  }

  useEffect(() => {
    if (dialogOpen) load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dialogOpen]);

  function open() {
    setError(null);
    setDialogOpen(true);
  }

  function close() {
    setDialogOpen(false);
  }

  async function confirmDelete() {
    const profile = pendingDelete;
    setPendingDelete(null);
    if (!profile) return;
    try {
      await client.deleteRepoProfile({ repoName: profile.repoName, name: profile.name });
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!name.trim()) return;
    setCreating(true);
    setError(null);
    try {
      await client.createRepoProfile({
        repoName,
        name,
        startCmd,
        toolKeys: tools,
        serviceIngredients: stateToServices(services),
      });
      setName("");
      setStartCmd("");
      setTools([]);
      setServices(servicesToState([]));
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  }

  return (
    <>
      <button type="button" onClick={open} className="btn btn-sm btn-ghost flex-none">
        Profiles
      </button>

      <Modal open={dialogOpen} onClose={close} boxClassName="max-w-2xl">
        <h3 className="font-semibold text-base mb-1">Environment recipes — {repoName}</h3>
        <p className="text-xs text-dim mb-3">
          Named profiles ("worker", "e2e", "lint", ...) a task's pod resolves its tools/services from.
        </p>

        <div className="flex flex-col">
          {profiles.map((p) => (
            <ProfileRow
              key={p.name}
              profile={p}
              onSaved={load}
              onRequestDelete={setPendingDelete}
              onError={setError}
            />
          ))}
          {profiles.length === 0 && <p className="text-sm text-dim py-2">No profiles configured yet.</p>}
        </div>

        <form onSubmit={handleCreate} className="flex flex-col gap-1.5 mt-3 pt-3 border-t border-base-content/10">
          <div className="flex items-center gap-2">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="profile name"
              className="input input-sm input-bordered w-28 flex-none"
              required
            />
            <input
              value={startCmd}
              onChange={(e) => setStartCmd(e.target.value)}
              placeholder="start command"
              className="input input-sm input-bordered flex-1 min-w-0 font-mono text-xs"
            />
            <button type="submit" disabled={creating || !name.trim()} className="btn btn-sm btn-primary flex-none">
              {creating ? "Adding…" : "Add"}
            </button>
          </div>
          <ToolAndServiceEditor tools={tools} setTools={setTools} services={services} setServices={setServices} />
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
        message={pendingDelete ? `Delete profile "${pendingDelete.name}" for ${repoName}?` : ""}
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
