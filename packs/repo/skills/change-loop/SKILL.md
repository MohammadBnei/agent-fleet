---
name: change-loop
description: How a change is made in ukubi's repos (pick the light or full loop by size, plan, doubt cycle, build, checks, fresh review, merge on green CI), the planning traps, and how a refactor proves it changed nothing. Use before writing code for any feature or refactor, or for a fix that crosses a module, changes a wire format or claims "behaviour unchanged".
---

# How a change is made: the ukubi dev rules

A mirror of mountain `plan/16-dev-rules.md` (revision 2, 2026-11-23) for pods that have no mountain checkout, re-synced by hand when plan/16 changes; plan/16 is authoritative. Pod deltas: **a fleet pod stops at the PR** (its Forgejo account is read-only: a reviewer and the owner merge, see the repo pack's Forgejo section), and a pod has no worktrees (one clone on its own PVC). The target repo's own CLAUDE.md wins on its conventions (test commands, gates, who merges).

## 1. Pick the loop by size
| Change | Loop |
|---|---|
| One bug, a few files, docs, a pin, static content | **Light loop** (section 2), inline, no workflow |
| Several slices, many files, security, auth, wire format, data, schema, or a "behaviour unchanged" claim | **Full loop** (section 3) |

The size decides the tool: a workflow is only for a large fan-out build. A change that fits both rows takes the full loop.

## 2. Light loop
1. A short plan in your head or the PR: what changes, what stays, each claim about today's code checked.
2. Edit, run the fast checks (typecheck, unit tests, `go test` on the touched packages), commit.
3. Open the PR and stop. A fresh review of its diff (section 3 step 9); a reviewer and the owner merge (section 4).

No workflow, no doubt agent, no interview. Anything the owner will see or feel still gets a hands-on look first (section 3 step 9).

## 3. Full loop
Main thread:
1. **Plan the whole batch holistically,** one plan, each claim about today's code with its `file:line`, every test it edits and every behaviour it changes.
2. **Interview the owner** on the open decisions, one question at a time. Owner decisions are settled here, never mid-build, and never relitigated.
3. **One fresh doubt agent** (read-only, not the plan's author) is given the plan and the code, not your conclusions, and tries to disprove the plan. Every numbered finding is taken or answered in the plan.

Workflow (only when the build fans out):
4. **Build fan-out.** Parallel builders on disjoint files, each in its own checkout. Builders run fast suites only. A separate checks lane writes the new checks and proves each red on the base; builders never write or weaken their own checks.
5. **Integrate once.** One agent merges the builders' work and regenerates generated output once.
6. **Checks once, then reviewers per angle.** Run the repo's disaster net once on the integrated tip, then one reviewer per angle (correctness, security, UI, over-engineering), never per slice.
7. **Fix:** one separate fixer owns every finding, looks for the structural cause first, and works in one agent with no nested agents or extra worktrees. At most 2 rounds, then back to the main thread. Review and fix stay separate.

Main thread:
8. **Open the PR,** one per repo per batch, with one Conventional Commit per concern, each revertable alone.
9. **A fresh review of the PR diff,** by an agent that did not build the change; it reads the diff with the repo, not a builder's report. Every finding is fixed or answered in the PR. For anything the owner will see or feel, look at it yourself on the final commit.
10. **Stop at the PR.** A reviewer and the owner merge it on green CI (section 4); infra-bootstrap is merged by the owner.

## 4. Checks, CI and merging
- **Tests guard disasters only** (no crash, the page boots, the world builds valid, the sim is deterministic): the whole net runs in under 60 s, in virtual time, and red means a real bug. No browser or GPU gate runs, no snapshot goldens of the world. Feel and UI detail are the owner's playthrough.
- **Builders run fast suites,** the full list runs once per round, mutations on new guards only. Check `uptime` before a fan-out: the host is shared.
- **Every new test gets a mutation:** disable the code it guards and see it fail for that reason. A test needs a positive control (a gate that only checks refusals passes on one that refuses everything).
- **CI** is Forgejo Actions; workflow files still live in `.github/workflows/`. PR checks run on `k8s-ubuntu-latest` (Talos, ib:ADR-0089). A job that needs `services:`, `container:` or root runs on `ubuntu-latest` (LXC podman runners, ib:ADR-0082), and image builds and deploys run on `ukubi-build` (the build-runner LXC). Watch CI with a deadline of 15 minutes or less (see the portable rules in PACK.md), never wait on an image build.
- **Merge (laptop and main-thread sessions only; a pod does not merge):** `POST /repos/<owner>/<repo>/pulls/<n>/merge` with `{"Do":"rebase"}`, pinning the PR's full 40-character head sha (`head_commit_id`, never a short one). A rebase merge keeps each concern's commit, so each stays revertable.
- **Commits:** Conventional Commit subjects, one commit per concern. Add no trailers and no AI attribution yourself. In a pod, `worker/git-hooks/commit-msg` stamps the fleet identity (`Co-Authored-By: ukubi-agent`, `Agent-Session`); leave it alone and add nothing.
- **A CI red for a reason outside the PR** (a runner down, an unset secret) is named in the PR and the PR is verified locally instead.

## 5. Planning traps
- **Check every fact against the code before writing it.** Plans have named the wrong version, the wrong test and a client that checks no HTTP status.
- **"Behaviour unchanged" and "tests unedited" are claims to prove.** List every test edit; a refactor may edit a test's setup, imports and renamed identifiers, never an assertion. If the design cannot keep the claim, change the design or drop the claim.
- **Order is behaviour:** registry order, map iteration, sort ties, log order and pass order are observable somewhere. Nil and empty differ on the wire. Error text is behaviour when something compares it.
- **Split and order the PRs:** a fix found during a refactor ships first, apart; a security change ships early.
- **Find the consumers outside the repo:** a log format, a metric name or a wire field may be read by another repo, an alert or a dashboard.
- **A fix covers every path to the state it guards.** List them before writing it.
- **Persisting bugs mean bad structure.** When a fix round stalls or one area keeps failing, name the duplicated owner or path (two owners of one state, one input handled twice), consolidate it, and delete the duplicate. Do not patch row by row.
- **Find the root cause, not a cosmetic patch** (a feel bug is reproduced first, then fixed where the thing is made).

## 6. Proving a refactor
- **The proof must reach the code being changed.** Break the code on purpose to see the proof fail. Build a golden from the old code and run it twice on main first: a harness that differs from itself proves nothing. Pin or mask randomness (map iteration, wall-clock fields, float sums over a map).
- **Drive the real path,** not a fake of something we own.
- **Methods that worked:** byte goldens of a transform's output; mutation harnesses for a build's refusals (compare exit codes and error classes, not text); dist byte checks under every engine CI runs; test names before and after with `git diff -M` of moved tests; old and new binaries side by side.
- **Run a harness that starts servers alone:** two at once collide on ephemeral ports.

## 7. Review
- Independent, and given the code, not your conclusions. Findings come back verified, with the path from a real input to the failure.
- Reviews keep finding: docs, comments and commit messages that claim more than the code does (re-read every sentence against the code); error paths that drop earlier errors; unchecked input; guards with a hole (write AST guards on `go/types`, one mutation per form); a gap dismissed as "covered by unit tests"; a binary committed by mistake (check `git status` before each commit).

## 8. Engines and tooling
- **Math is the platform's:** Bun calls the host's libm, Node ships its own. A comparator that never returns 0 leaves ties in an engine-defined order; sort in one written-down order.
- **Bun 1.4:** `const`/`let` become `var`; tree-shaking shifts three.js object ids; `--chunk-naming '[name].[ext]'` fails when a chunk and an entry share a name (keep `[hash]`); hashed chunk names depend on the working directory; `bun build` does not clean its outdir; a stray `.js` beats its `.ts`; `bun test tools` is a substring filter (write `./tools`); `bun add` writes `^` ranges (use `-E`). The dist is built with the CI-pinned Bun exactly.
- **TypeScript 6** defaults `types` to `[]`.
- **Biome** lints and formats JS and TS in every repo that has any: pinned exactly, one root `biome.json`, `biome ci .` in CI, in place of ESLint and Prettier.
- **Processes:** wait on explicit PIDs, never a bare `wait`; never `pkill -f`. Cleanup after browser and stack runs: see the portable rules in PACK.md.
- **Live pins use frozen refs** (a sha), never a branch that moves.
- **Agents share the checkout:** say in a reviewer's brief which edits are yours.

---
Source: mountain `plan/16-dev-rules.md`, revision 2 (2026-11-23); the file there is authoritative. OPEN: pin the merged mountain sha here once the plan/16 PR has merged.
