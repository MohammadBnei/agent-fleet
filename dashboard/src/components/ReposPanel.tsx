import { useEffect, useMemo, useRef, useState } from "react";
import { client } from "../connectClient";
import type { Repo, SyncRepoResponse } from "../gen/agentfleet/v1/dashboard_pb";
import {
  filterRepos,
  isNew,
  keepShown,
  ownerOf,
  type RepoFilter,
  type RepoStatus,
  toggleAll,
} from "../repoFilter";
import { useToggleSet } from "../useToggleSet";
import { ConfirmModal } from "./ConfirmModal";
import { InlineError } from "./InlineError";

// The fleet's target repos (docs/adr/0028), projected from GitHub plus
// infra-bootstrap's overrides file since docs/adr/0074. The installation is
// what the fleet can reach; `active` is what it uses, and the one thing this
// panel writes. An inactive repo starts no new session.
//
// ponytail: a plain scrolling list, ~200 rows render fine; virtualize if it
// reaches thousands.

// What a finished sync says. The point of the button is answering "did that
// do anything?", so a bare "done" would waste the whole round trip — the
// provisioner already counted the commits and timed itself.
//
// "already current" is the one claim here that can be wrong, so it is the last
// branch rather than the fallback. commitsAdvanced is 0 in two cases that are
// not "nothing moved": no resolvable origin/HEAD to measure against (which is
// every cache cloned before that symref was recorded, i.e. exactly the stale
// ones someone is most likely to sync), and a force-push that rewound the
// branch. Both are reported as movement of unknown size instead.
function syncLabel(res: SyncRepoResponse): string {
  const secs = `${(Number(res.durationMs) / 1000).toFixed(1)}s`;
  const head = res.head ? ` · ${res.head.slice(0, 7)}` : "";
  if (res.cloned) return `cloned${head} · ${secs}`;
  if (res.commitsAdvanced > 0) {
    return `+${res.commitsAdvanced} commit${res.commitsAdvanced === 1 ? "" : "s"}${head} · ${secs}`;
  }
  if (res.headChanged) return `head moved${head} · ${secs}`;
  if (!res.head) return `fetched, extent unknown · ${secs}`;
  return `already current · ${secs}`;
}

// One repo's last sync outcome. Kept per-row rather than in the modal's single
// `error` line so a failed sync of one repo during "Sync all" doesn't erase
// the result of the three that worked.
type SyncState = { text: string; failed: boolean };

// A bulk deactivation past this many repos asks first.
const CONFIRM_OVER = 10;

function RepoRow({
  repo,
  isNewRepo,
  selected,
  onSelect,
  onActive,
  writing,
  onSync,
  syncing,
  busy,
  sync,
}: {
  repo: Repo;
  isNewRepo: boolean;
  // An active write is in flight: the switch waits for its reload.
  writing: boolean;
  selected: boolean;
  onSelect: () => void;
  onActive: (active: boolean) => void;
  onSync: (repo: Repo) => void;
  // This row is the one in flight (label only).
  syncing: boolean;
  // Some row is in flight — possibly another one, mid "Sync all". Disabling
  // only the in-flight row let a second click land, and its finally-clause
  // cleared the shared indicator, re-enabling "Sync all" for a second
  // concurrent loop over the same repos.
  busy: boolean;
  sync: SyncState | undefined;
}) {
  return (
    <div
      className={`py-1 border-b border-base-content/5 last:border-0 ${repo.active ? "" : "opacity-60"}`}
    >
      <div className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={selected}
          onChange={onSelect}
          aria-label={`select ${repo.name}`}
          className="checkbox checkbox-xs flex-none"
        />
        <input
          type="checkbox"
          role="switch"
          checked={repo.active}
          aria-checked={repo.active}
          onChange={(e) => onActive(e.target.checked)}
          disabled={writing}
          aria-label={`${repo.name} active`}
          title={
            repo.active
              ? "Active: new sessions allowed"
              : "Inactive: no new sessions"
          }
          className="toggle toggle-xs toggle-success flex-none"
        />
        <span
          className="text-sm font-medium w-36 truncate flex-none"
          title={repo.name}
        >
          {repo.name}
        </span>
        <span
          className="text-xs text-dim flex-1 min-w-0 truncate"
          title={repo.url}
        >
          {repo.fullName || repo.url}
        </span>
        {isNewRepo && (
          <span
            className="badge badge-sm badge-info flex-none"
            title="Seen by the sync in the last 7 days"
          >
            new
          </span>
        )}
        {repo.clusterAccess && (
          <span
            className="badge badge-sm badge-warning flex-none"
            title="Sessions get the kubectl shim (docs/adr/0037)"
          >
            cluster
          </span>
        )}
        <span className="text-xs flex-none hidden sm:inline">
          {repo.baseBranch || "main"}
        </span>
        {repo.image && (
          <span
            className="text-xs text-dim w-32 truncate flex-none hidden sm:inline"
            title={repo.image}
          >
            {repo.image}
          </span>
        )}
        {/* Refreshes the provisioner's clone cache for this repo. Nothing else
          advances it between pod creations, so a new repo has no cache at all
          until its first session pays for a cold clone. */}
        <button
          type="button"
          onClick={() => onSync(repo)}
          disabled={busy || !repo.active}
          title={
            repo.active
              ? "Fetch this repo into the fleet's shared clone cache"
              : "Inactive repos are not synced"
          }
          className="btn btn-xs flex-none"
        >
          {syncing ? "Syncing…" : "Sync"}
        </button>
      </div>
      {sync && (
        <p
          className={`text-xs mt-1 ${sync.failed ? "text-error" : "text-dim"}`}
        >
          {sync.text}
        </p>
      )}
    </div>
  );
}

export function ReposPanel() {
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState<string | null>(null);
  const [syncState, setSyncState] = useState<Record<string, SyncState>>({});
  const [filter, setFilter] = useState<RepoFilter>({
    query: "",
    owner: "",
    status: "all",
  });
  const {
    set: selected,
    toggle: select,
    setSet: setSelected,
    clear: clearSelected,
  } = useToggleSet();
  // A count, not a flag: two overlapping writes must not end "writing" when
  // the first one finishes.
  const [writes, setWrites] = useState(0);
  const writing = writes > 0;
  // Only the latest reload may land: a slow one must not overwrite a newer list.
  const loadSeq = useRef(0);
  const [pendingDeactivate, setPendingDeactivate] = useState<string[] | null>(
    null,
  );

  function load() {
    const seq = ++loadSeq.current;
    return client
      .listRepos({})
      .then((res) => {
        if (seq !== loadSeq.current) return;
        setRepos(res.repos);
        setLoaded(true);
      })
      .catch((err: Error) => {
        setError(err.message);
        setLoaded(true);
      });
  }
  // "No repos configured yet." is a claim, so it waits for the first answer.
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    load();
  }, []);

  // Mount time: "new" is a 7-day window, so a stale clock costs nothing.
  const [now] = useState(() => Date.now());
  const owners = useMemo(
    () => [...new Set(repos.map(ownerOf).filter(Boolean))].sort(),
    [repos],
  );
  const shown = filterRepos(repos, filter, now);
  // Bulk actions act on what is both selected and shown: a row the filter
  // hides is never switched off by a click the operator could not see.
  // updateFilter already drops hidden rows; this covers a reload removing one.
  const picked = shown.filter((r) => selected.has(r.name));
  const allShownPicked = shown.length > 0 && picked.length === shown.length;

  function updateFilter(patch: Partial<RepoFilter>) {
    const next = { ...filter, ...patch };
    setFilter(next);
    setSelected((prev) => keepShown(prev, filterRepos(repos, next, now)));
  }
  const activeCount = repos.filter((r) => r.active).length;
  const newCount = repos.filter((r) => isNew(r, now)).length;

  // Reload rather than patch locally: the server skips gone and unknown
  // names, and the list is what it says.
  async function setActive(names: string[], active: boolean) {
    setWrites((n) => n + 1);
    setError(null);
    clearSelected();
    try {
      await client.setReposActive({ names, active });
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setWrites((n) => n - 1);
    }
  }

  // Only rows that would actually change are sent, and counted for the confirm.
  function bulk(active: boolean) {
    const changing = picked
      .filter((r) => r.active !== active)
      .map((r) => r.name);
    if (changing.length === 0) clearSelected();
    else if (!active && changing.length > CONFIRM_OVER)
      setPendingDeactivate(changing);
    else void setActive(changing, active);
  }

  // The error message is shown verbatim rather than reduced to "sync failed":
  // core forwards git's own stderr ("fatal: could not read Username",
  // "Repository not found"), which is the entire diagnostic available.
  async function syncOne(repo: Repo) {
    setSyncing(repo.name);
    try {
      const res = await client.syncRepo({ name: repo.name });
      setSyncState((prev) => ({
        ...prev,
        [repo.name]: { text: syncLabel(res), failed: false },
      }));
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setSyncState((prev) => ({
        ...prev,
        [repo.name]: { text: msg, failed: true },
      }));
    } finally {
      setSyncing(null);
    }
  }

  // Sequential on purpose: the provisioner serializes per-repo git work behind
  // its own mutex anyway, and one-at-a-time keeps each repo's result attached
  // to its own row for free. Inactive repos are skipped; core refuses them.
  async function syncAll() {
    for (const repo of repos.filter((r) => r.active)) {
      await syncOne(repo);
    }
  }

  const field = "input input-sm input-bordered";
  return (
    <>
      <div className="flex flex-col">
        <div className="flex items-center justify-between gap-2 mb-3">
          <div className="flex items-baseline gap-2 min-w-0">
            <h3 className="font-semibold text-base">Repos</h3>
            <span className="text-xs text-dim truncate">
              {activeCount} active · {repos.length - activeCount} inactive ·{" "}
              {newCount} new
            </span>
          </div>
          <button
            type="button"
            onClick={syncAll}
            disabled={syncing !== null || activeCount === 0}
            title="Fetch every active repo into the fleet's shared clone cache"
            className="btn btn-sm flex-none"
          >
            {syncing ? `Syncing ${syncing}…` : "Sync all"}
          </button>
        </div>

        {/* w-auto: DaisyUI sizes inputs and selects at 20rem, which wrapped
            this row at 1280px. */}
        <div className="flex flex-wrap sm:flex-nowrap items-center gap-2 mb-2">
          <input
            type="search"
            value={filter.query}
            onChange={(e) => updateFilter({ query: e.target.value })}
            placeholder="search repos"
            aria-label="search repos"
            className={`${field} w-auto flex-1 min-w-0`}
          />
          <select
            value={filter.owner}
            onChange={(e) => updateFilter({ owner: e.target.value })}
            aria-label="owner"
            className="select select-sm select-bordered w-auto flex-none"
          >
            <option value="">all owners</option>
            {owners.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
          <select
            value={filter.status}
            onChange={(e) =>
              updateFilter({ status: e.target.value as RepoStatus })
            }
            aria-label="status"
            className="select select-sm select-bordered w-auto flex-none"
          >
            <option value="all">all</option>
            <option value="active">active</option>
            <option value="inactive">inactive</option>
            <option value="new">new</option>
          </select>
        </div>

        <div className="flex items-center gap-2 py-1 border-b border-base-content/10">
          <input
            type="checkbox"
            checked={allShownPicked}
            onChange={() => setSelected((prev) => toggleAll(prev, shown))}
            disabled={shown.length === 0}
            aria-label={`select all ${shown.length} shown`}
            className="checkbox checkbox-xs flex-none"
          />
          <span className="text-xs text-dim flex-1">
            {picked.length > 0
              ? `${picked.length} of ${shown.length} selected`
              : `${shown.length} shown`}
          </span>
          <button
            type="button"
            className="btn btn-xs"
            disabled={writing || picked.length === 0}
            onClick={() => bulk(true)}
          >
            Activate
          </button>
          <button
            type="button"
            className="btn btn-xs"
            disabled={writing || picked.length === 0}
            onClick={() => bulk(false)}
          >
            Deactivate
          </button>
        </div>

        <div className="flex flex-col max-h-[60vh] overflow-y-auto">
          {shown.map((r) => (
            <RepoRow
              key={r.name}
              repo={r}
              isNewRepo={isNew(r, now)}
              selected={selected.has(r.name)}
              onSelect={() => select(r.name)}
              onActive={(active) => void setActive([r.name], active)}
              writing={writing}
              onSync={syncOne}
              syncing={syncing === r.name}
              busy={syncing !== null}
              sync={syncState[r.name]}
            />
          ))}
          {shown.length === 0 && (
            <p className="text-sm text-dim py-2">
              {!loaded
                ? "Loading…"
                : repos.length === 0
                  ? "No repos yet."
                  : "No repos match."}
            </p>
          )}
        </div>

        <p className="text-xs text-dim mt-3 pt-3 border-t border-base-content/10">
          To add a repo, invite <code>argocd-ukubi-bot</code> as a collaborator;
          it shows up within two minutes, active. Remove the bot to remove it.
          An inactive repo starts no new session; its existing sessions keep
          working. Name, base branch, image and cluster access are set in
          infra-bootstrap's{" "}
          <code>gitops/bootstrap/fleet-repo-overrides.yaml</code>.
        </p>

        <InlineError message={error} />
      </div>

      <ConfirmModal
        open={pendingDeactivate !== null}
        title={`Deactivate ${pendingDeactivate?.length ?? 0} repos?`}
        message="They start no new sessions until switched back on. Sessions already open keep working."
        confirmLabel="Deactivate"
        onConfirm={() => {
          const names = pendingDeactivate ?? [];
          setPendingDeactivate(null);
          void setActive(names, false);
        }}
        onCancel={() => setPendingDeactivate(null)}
      />
    </>
  );
}
