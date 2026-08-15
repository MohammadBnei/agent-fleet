# Plan — the dashboard has not caught up with the session model

**For whoever picks this up.** Self-contained; you should not need this
conversation's history. Read `docs/adr/0048-one-session-one-pod-one-shared-home.md`
§1 before starting — everything below follows from it.

Delete this file in the PR that finishes the work.

## Context

`docs/adr/0048` changed the unit of work from a **task** to a **session**, and
shipped as **v3.0.0** on 2026-08-15. The dashboard was renamed only far enough
to compile against the new proto. Its copy, its file and symbol names, and —
most importantly — **its create flow** still implement the model that was
replaced.

The dashboard is the primary interface (`docs/adr/0029` demoted Discord to
notification-only), so this is not cosmetic drift. P0 below is a live bug.

## P0 — Creating a session silently does nothing

**This is the headline. Do it first, and do not let the rename hide it.**

What happens today when a human starts work:

| Step | Code | Effect |
|---|---|---|
| Dialog refuses to submit without a description | `dashboard/src/components/NewTaskDialog.tsx:75` | forces the human to type an instruction |
| Sends it as `createSession({repo, description})` | `NewTaskDialog.tsx:79` | |
| `CreateSession` writes a **row only** | `core/internal/dashboard/server.go:133-158` | no pod, no transcript entry, no warm |
| `description` is a label | `db/migrations/000001_init.up.sql:66-71` | *"Human-facing labels only. Deliberately never part of the prompt"* |

So the human types an instruction, it lands in a column the agent never reads,
no pod boots, and the session sits idle forever. Nothing errors. The only
symptom is that the fleet appears to ignore you.

### The model it must implement instead (ADR-0048 §1)

```
CreateSession(repo, title?)   -> row only. No pod, no directory.
SendMessage(id, text)         -> if no live pod: provision, THEN append.
```

**A session with no message is a valid resting state.** That is deliberate: it
is the structural human gate — nothing machine-initiated produces a message, so
nothing machine-initiated produces a pod.

### What to build

- **Drop the description requirement.** Repo (plus the cluster-access checkbox)
  is enough to create a session. "Create empty, then talk to it" must work.
- **Optional first message.** If the human types one, the flow is
  `CreateSession` → **`SendMessage`** — *not* create-with-description.
- **Ordering is load-bearing and easy to get wrong.** Warming computes
  `resumeFromSeq = LatestSeq`; a message appended *before* the pod exists lands
  below that cursor and is never delivered. Warm first, append second.
  `PostMessage`, `OpenFromProposal` and `PromptSession` in
  `core/internal/dashboard/server.go` and `core/internal/coreserver/interagent.go`
  each carry this comment — `PromptSession` had the bug and was fixed in
  v3.0.0. Copy the pattern; do not re-derive it.
- **`title` may carry a human label.** Never send an instruction as
  `description`.
- **Render the empty session.** It has no pod, no transcript, no `liveState`.
  Confirm it lands in exactly one bucket — `bucketTasks` is a partition and
  `dashboard/src/bucketTasks.test.ts` asserts every session lands in exactly
  one. A session matching no bucket does not sort oddly, it **vanishes from the
  list entirely**.

### While you are in that file

`snippetIds` is fetched, rendered as checkboxes, toggled — and **never sent
anywhere** (`NewTaskDialog.tsx:30, 61, 81`; the `createSession` call at `:79`
omits it). ADR-0048 said prompt snippets would prefill the composer instead;
that was never implemented. Either wire them into the composer prefill or
delete the picker. A control that silently does nothing is worse than no
control.

## P1 — Rename task → session, in one pass

Copy, URL, files and symbols together. Decided deliberately: a half-rename
leaves the dashboard permanently inconsistent with `core/internal/sessions`.

**User-visible** (small, and the part that matters most):

| File | What |
|---|---|
| `dashboard/src/App.tsx:47,55` | nav label `tasks` — desktop and mobile |
| `NewTaskDialog.tsx:96` | `aria-label="Send a task"` |
| `NewTaskDialog.tsx:103` | `+ send a task` |
| `NewTaskDialog.tsx:107` | `New task` heading |
| `dashboard/public/icons/site.webmanifest:49` | PWA shortcut `New task` |
| `App.tsx:27,181,182` | URL `?task=<id>` |

**Keep reading `?task=` as a fallback** while writing `?session=`. Existing
bookmarks and every link already shared must keep resolving. Three lines; do
not skip it.

**Internal:** `TaskList.tsx`, `TaskDetail.tsx`, `NewTaskDialog.tsx`,
`MobileTaskList.tsx`, `MobileTaskDetail.tsx`, `useTaskDetail`, `loadTasks`,
`selectTask`, `deleteTask`, `readTaskIdFromUrl`, the `View` union's `"tasks"`
member, and so on.

**Do not touch** `dashboard/src/gen/**` — generated from `proto/`. If a proto
field still says task, that is a separate decision with a `buf breaking`
consequence; leave it and say so in the PR.

## P2 — Run `/dashboard-e2e`, fix what it finds

The skill exists and has **never been run against the rewritten console**
(`docs/adr/0042`/`0043`). It stands up a throwaway Postgres + `core` + the Vite
dev server, drives Playwright, and tears everything down.

Cover at minimum:

- create an **empty** session — no pod, row visible in the list
- create **with** a first message — assert it reaches the transcript
  **verbatim**, no wrapper
- answer a permission from the list (`DecisionInline`) *and* from
  `DecisionDock`
- the three-way DENSITY control
- archive
- both form factors — desktop and mobile share `SessionFeed`/`SessionPanels`/
  `DecisionInline`/`DecisionDock`

**Trap:** specs must be state-idempotent. Answering a permission *consumes* it,
so a re-run against a dirty database is not the same test.

## P3 — Bundle size

`bun run build` warns. Current chunks: `index` **687 KB**, `cytoscape`
**434 KB**, `katex` **258 KB**. The SPA is compiled into `core`'s binary, so
this is first-visit load — including on the phone the PWA targets.

- `cytoscape` is used only by the observability view's `FleetTopology`
  (`docs/adr/0047`). Lazy-load that route.
- `katex` is only for maths in rendered markdown. Lazy-load it.

Target: the default route pulls neither.

## P4 — Smaller things, already verified

- `App.tsx`'s `counts` and `bucketTasks` key on `liveState === "blocked"`, not
  the raw `pendingDecisions` count. **Keep that invariant.** The server derives
  blocked as live-AND-pending, so the count alone disagrees by construction for
  a session whose pod is gone — that session would pin itself to the top of the
  list forever, offering allow/deny buttons that reach nothing.
- The repos modal describes `repos.image` in both the edit row and the create
  form. There is no e2e pod any more; do not reintroduce "profile" language.

## Verification

- `cd dashboard && bun run build` — **not** `tsc --noEmit`. `tsc -b` enforces
  `noUnusedLocals`, and the weaker check has shipped a broken `core` image
  before (see `CLAUDE.md`'s "Verification traps").
- `cd dashboard && bun run test` — 52 tests. The `bucketTasks` partition tests
  must stay green; if one fails, fix the code, not the assertion.
- `/dashboard-e2e` green.
- **The one that actually proves P0**, and it needs a real cluster: create a
  session with no message, confirm no pod exists
  (`kubectl get pods -n agent-fleet`) and the row renders. Then send a message
  and confirm a pod boots and the message arrives verbatim in the transcript.
  Green CI cannot tell you this.
- `grep -rn task dashboard/src | grep -v gen/` → nothing but the `?task=`
  back-compat read.

## Ground rules

Feature branch + PR, Conventional Commits (`release-it` cuts the version from
them). Never edit `docs/ARCHITECTURE.md`, `docs/DECISIONS.md` or `docs/adr/` to
match your work — propose the change explicitly instead. `CLAUDE.md`'s
"Verification traps" section is short and every entry in it cost a real
incident; read it before claiming done.
