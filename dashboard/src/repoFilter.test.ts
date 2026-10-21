import { test, expect } from "bun:test";
import type { Repo } from "./gen/agentfleet/v1/dashboard_pb";
import { activeRepos, filterRepos, isNew, keepShown, ownerOf, toggleAll } from "./repoFilter";

const now = Date.parse("2026-10-21T00:00:00Z");

function repo(name: string, over: Partial<Repo> = {}): Repo {
  return { name, fullName: `MohammadBnei/${name}`, active: true, createdAt: "2026-01-01T00:00:00Z", ...over } as Repo;
}

const repos = [
  repo("blog", { createdAt: "2026-10-18T00:00:00Z" }),
  repo("notes", { active: false }),
  repo("workspace", { fullName: "opus-tech-hq/workspace" }),
];

test("isNew is the last 7 days", () => {
  expect(isNew(repos[0], now)).toBe(true);
  expect(isNew(repos[1], now)).toBe(false);
  expect(isNew(repo("x", { createdAt: "" }), now)).toBe(false);
});

test("ownerOf reads full_name", () => {
  expect(ownerOf(repos[2])).toBe("opus-tech-hq");
  expect(ownerOf(repo("x", { fullName: "" }))).toBe("");
});

test("filters compose: query, owner, status", () => {
  const names = (f: Parameters<typeof filterRepos>[1]) => filterRepos(repos, f, now).map((r) => r.name);
  expect(names({ query: "", owner: "", status: "all" })).toEqual(["blog", "notes", "workspace"]);
  expect(names({ query: "OPUS", owner: "", status: "all" })).toEqual(["workspace"]);
  expect(names({ query: "", owner: "MohammadBnei", status: "all" })).toEqual(["blog", "notes"]);
  expect(names({ query: "", owner: "", status: "inactive" })).toEqual(["notes"]);
  expect(names({ query: "", owner: "", status: "active" })).toEqual(["blog", "workspace"]);
  expect(names({ query: "", owner: "", status: "new" })).toEqual(["blog"]);
});

test("toggleAll selects every filtered row, then clears only those", () => {
  const shown = repos.slice(0, 2);
  const picked = toggleAll(new Set(["workspace", "blog"]), shown);
  expect([...picked].sort()).toEqual(["blog", "notes", "workspace"]);
  expect([...toggleAll(picked, shown)]).toEqual(["workspace"]);
});

test("keepShown drops selected rows the filter now hides", () => {
  expect([...keepShown(new Set(["blog", "notes", "gone"]), repos.slice(0, 1))]).toEqual(["blog"]);
});

test("activeRepos keeps active rows plus the one named", () => {
  expect(activeRepos(repos).map((r) => r.name)).toEqual(["blog", "workspace"]);
  expect(activeRepos(repos, "notes").map((r) => r.name)).toEqual(["blog", "notes", "workspace"]);
});
