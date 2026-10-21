import type { Repo } from "./gen/agentfleet/v1/dashboard_pb";

// The repo manager's filter and select-all logic, pure so it is testable
// without rendering ~200 rows.

export type RepoStatus = "all" | "active" | "inactive" | "new";
export type RepoFilter = { query: string; owner: string; status: RepoStatus };

const NEW_MS = 7 * 24 * 60 * 60 * 1000;

// created_at is when the GitHub sync first saw the repo.
export function isNew(r: Repo, now: number): boolean {
  const t = Date.parse(r.createdAt);
  return !Number.isNaN(t) && now - t < NEW_MS;
}

export function ownerOf(r: Repo): string {
  return r.fullName.split("/")[0];
}

export function filterRepos(repos: Repo[], f: RepoFilter, now: number): Repo[] {
  const q = f.query.trim().toLowerCase();
  return repos.filter(
    (r) =>
      (!q || r.name.toLowerCase().includes(q) || r.fullName.toLowerCase().includes(q)) &&
      (!f.owner || ownerOf(r) === f.owner) &&
      (f.status === "all" ||
        (f.status === "active" && r.active) ||
        (f.status === "inactive" && !r.active) ||
        (f.status === "new" && isNew(r, now))),
  );
}

// The header checkbox: when every shown row is already selected it clears
// them, otherwise it adds them. Rows hidden by the filter keep their state.
export function toggleAll(selected: Set<string>, shown: Repo[]): Set<string> {
  const next = new Set(selected);
  const all = shown.length > 0 && shown.every((r) => next.has(r.name));
  for (const r of shown) {
    if (all) next.delete(r.name);
    else next.add(r.name);
  }
  return next;
}
