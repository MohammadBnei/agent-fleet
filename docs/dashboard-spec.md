# agent-fleet dashboard — complete feature inventory

Written as input to a UI rewrite. This is *what the interface must be able
to express*, not how it currently looks. Everything below is live today.

## What this product is

A control room for a fleet of autonomous Claude Code sessions. Each session
owns one task end-to-end (plan → code → tests → PR) on a real repo, runs in
its own Kubernetes pod, and can stop at any moment to ask a human a
question or for permission to do something. The dashboard is the primary
surface — not Discord, not a terminal.

**The one job this UI has:** across N sessions running unattended, make it
instantly obvious *which one needs me, and why*. Everything else is
secondary.

Three consequences worth designing around:

- **Sessions are long-lived and mostly unattended.** A user opens this
  after an hour away. "What happened while I was gone" matters more than
  live streaming.
- **Blocking states are the payload.** A session waiting on a permission
  decision is stalled until a human clicks. Latency here is the product's
  real cost.
- **The feed is machine-generated and dense.** One session produces
  hundreds of entries of ~12 distinct kinds. Uniform treatment makes it
  unreadable; that's the core design problem.

---

## 1. Screens

| Screen | Purpose |
|---|---|
| **Task list** (primary) | Every session, filterable. The "which one needs me" view. |
| **Task detail** | One session: transcript feed, blocking cards, actions, side panels. Where all the time is spent. |
| **Worktrees** | Git worktrees on the shared volume — orphan/cleanup management. |
| **Files** | A flat fleet-wide shared file space (S3), shared by every session and the human. |
| **Audits** | Scheduled recurring tasks (cron-like). |
| Modals | Manage repos · repo profiles (pod recipes) · prompt snippets (reusable guidance) · scheduled audits · new task · confirm/bypass/error |

Desktop and mobile are both first-class. Mobile has no sidebar, so
everything the sidebar carries must have a home there.

---

## 2. The session object

Each session carries:

`id` · `repo` · `description` · `kind` (`worker` | `thot` = cluster-agent) ·
`status` · `live_state` · `pod_phase` · `pod_message` · `heartbeat_at` ·
`last_active_at` · `retry_count` · `last_error` · `pr_url` · `branch` ·
`permission_mode` · `awaiting_human` · `session_id`

**Two orthogonal state axes — this is the single most important modelling
fact for the UI, and the current design conflates them:**

**A. Workflow status** — where the task is in its life:
`proposed` (machine-created, needs human approval) · `pending` · `claimed` ·
`running` · `done` · `failed` · `cancelled` · `failed_permanently`

**B. Live state** — what the session is doing *right now*:

| State | Meaning | Design weight |
|---|---|---|
| `blocked` | Waiting on a **human** decision | **Loudest thing on screen.** This is the product. |
| `stalled` | Owes a reply and hasn't produced one | Warning — offer interrupt/kill |
| `done` | Finished **while nobody was looking** | Prominent — the "welcome back" state |
| `working` | Actively working | Calm, alive |
| `idle` | Finished, and seen | Quiet |
| `unknown` | Pod up, agent hasn't spoken yet | Transient, starting |
| `""` | No live pod | Neutral |

A session can be `running` + `blocked`, or `running` + `stalled`. Both axes
must be readable at a glance, and they are not the same badge.

**Pod phase** is a third, lesser axis: `PROVISIONING` (carries a live
sub-step message: cloning / adding worktree / creating pod) · `CREATED` ·
`SCHEDULED` · `RUNNING` · `SUCCEEDED` · `CRASHED` · `TERMINATED`.

Plus a **stale** signal: heartbeat older than threshold → will be reclaimed
and redispatched.

---

## 3. The transcript feed — 12 entry kinds

The heart of the UI. Each kind carries genuinely different information and
deserves genuinely different visual weight. Rough hierarchy:

### Tier 1 — demands action (full-width, unmissable)

- **Permission request.** The agent wants to run a tool. Shows the tool and
  its input *rendered per tool*: an `Edit` as a real line diff, a `Write` as
  its content, a `Bash` as its command. Actions: **Allow** / **Deny** (+
  optional inline reason, single click each). Once resolved, collapses to a
  one-line outcome — allowed / denied *with the reason* / interrupted.
- **Plan card.** `ExitPlanMode`'s special case: the agent's full plan as
  markdown. Actions: **Approve** or **Request changes** (free text).
  Desktop also supports selecting passages of the plan to annotate, folded
  into one feedback message.
- **Question card.** Structured multiple-choice from the agent
  (1..n questions, each with header, prompt, options with descriptions,
  single- or multi-select). Renders as a form; also surfaces as quick-reply
  chips near the composer for the simple single-question case. Shows the
  submitted answer once answered.

### Tier 2 — narrative (the readable conversation)

- **Agent prose** (markdown, incl. mermaid diagrams).
- **Human messages** (visually distinct — currently a blockquote).
- **Messages from another session** — agents can now prompt each other;
  these arrive attributed (`[from session <id>]`).
- **Thinking blocks** — the agent's reasoning. Collapsed by default.

### Tier 3 — tool activity (dense, scannable, collapsible)

- **Tool call + result pairs.** One line: status dot (in-flight / ok /
  error), tool name, a one-line summary of the input, and a result summary
  (`35 lines`, `−3 +4`, `failed`). Expandable to show **the full input**
  (with diffs for Edit/Write) **and** the result. In-flight calls spin.
- **Tool progress** — "Bash still running · 32s" for long calls.
- **File-change telemetry** — periodic `{branch, files[+added/−removed]}`.

### Tier 4 — session lifecycle (quiet log lines)

- **Session init** — the environment: model, permission mode, cwd, CLI
  version, tool count, skills, plugins, **MCP servers with status**
  (a failed server silently removes tools — must be visible).
- **Result** — end of a turn: turns · cost · wall/API duration · token
  counts (in/out/cache) · per-model cost breakdown · permission denials ·
  errors.
- **Context compacted** — a boundary marker with pre-compaction token
  count.
- **Hook output** — stdout/stderr/exit code from lifecycle hooks.
- **Permission-mode change**, **approved**, **killed**, **interrupted** —
  backend-verified events (distinct from the agent *claiming* one happened).

### Tier 5 — alarms (rare, loud)

- **Authentication failed** (expired OAuth token)
- **Model error** (rate limit, billing, server error)
- **Resume failed** — the session was meant to resume and silently started
  fresh with no history

These are the "why has it gone quiet" answers. They must not look like log
lines.

---

## 4. Actions

**On a live session:** send a message (free text, with slash-command
autocomplete from the session's real command list) · answer a question ·
allow/deny a permission · approve a plan / request changes · **interrupt**
(stop the current turn, pod survives) · **kill** (end the session) ·
**warm** (boot a pod for an idle session) · change permission mode
(default / plan / accept-edits / **bypass** — bypass requires typing a
confirmation word) · open code-server (browser IDE into the live worktree)
· kill the e2e preview environment.

**On a proposed session:** approve & dispatch · dismiss.

**Anywhere:** create a task (repo picker, description, prompt-snippet
picker, cluster-session toggle) · delete a session · filter/search
sessions · toggle showing tools/changes inline · manage repos, repo
profiles, prompt snippets, scheduled audits · upload/download/delete shared
files · browse and delete worktrees.

---

## 5. Side panels (desktop) / equivalents (mobile)

- **TODOS** — the agent's own live todo list (from its `TodoWrite` calls),
  with progress. Mobile renders it as a thin progress bar.
- **TOOL CALLS** — every tool call, chronological.
- **CHANGES** — current branch + per-file `+added/−removed`.
- **E2E preview card** — live app URL, pod phase, readiness, restarts,
  uptime, the resolved start command, profile name, tools and services.

Panels are collapsible, drag-resizable, with a "fit height" mode. The
sidebar itself is drag-resizable. All persisted.

---

## 6. Live behaviour

- Transcript arrives over a **server stream** with a resume cursor;
  reconnects with backoff, pauses when the tab is hidden.
- Task list polls every 5s. E2E status polls every 5s.
- Optimistic echo for a sent message (shown greyed with a spinner until
  confirmed).
- Auto-scroll rules: a human's own message always jumps to bottom; an agent
  message only if already at the bottom, otherwise a pulsing
  "jump to bottom" affordance.
- Opening a session **marks it seen** — this is what turns `done` back into
  `idle`, so it must be a real open, not a hover.
- Global header count: "N waiting on you".

---

## 7. What's deliberately absent (don't design for it)

No token-by-token streaming (turns arrive whole). No per-entry timestamps
on the wire. No editing/rewinding history, no forking a session, no model
switcher, no file attachments to the agent, no `@`-file mention, no
"always allow this tool".

---

## 8. Design brief

Where the current UI falls down, i.e. what a rewrite should fix.

> **All six are now addressed** by the console rewrite — see
> [`adr/0042`](adr/0042-console-rewrite.md), which implements
> `Agent Fleet Console.dc.html` and `Agent Fleet Console Mobile.dc.html`. The
> items are kept below, struck through, because each records *why* a piece of the
> current design exists; deleting them would lose the reasoning. Everything above
> §8 is still the live contract for what the interface must express.

1. ~~**The two state axes are visually conflated.**~~ **Fixed.**
   Status, live state, pod phase and staleness used to render as four
   badges of near-identical weight, producing pairs that restated each
   other (`CANCELLED TERMINATED`) or outright contradicted each other
   (`SCHEDULED DONE`). They are now a single precedence-ranked badge
   (`sessionBadge` in `pages/TaskList.tsx`), ordered by what a human needs
   to know first: needs-you → crashed → stale → stalled → done → in-motion
   → workflow status. Demoted detail survives in the tooltip. The ranking was
   kept verbatim and given real weight; `sessionBadge.test.ts` still passes
   untouched, which is the proof it survived the rewrite.
2. ~~**The feed has almost no visual hierarchy.**~~ **Fixed.** A `$0.42 ·
   7 turns` result line, a compaction marker, and an agent's actual prose were
   all ~10px grey text — Tier 1 and Tier 4 looked the same. `SessionFeed` now
   renders the five tiers differently: full-width cards, readable prose, dense
   grouped tool rows, hairline lifecycle rules, orange alarm bars.
3. ~~**Blocking cards don't dominate.**~~ **Fixed, and then some.** The pending
   decision is now rendered *and answerable in the list* — an `Edit` as a real
   diff with allow/deny — so unblocking a session costs no navigation at all. On
   mobile it docks above the composer rather than scrolling away inline.
4. ~~**Density has no control.**~~ **Fixed.** A three-way density control, a
   decision spine with jump-to-entry and `↓ next decision`, and collapse-all on
   each grouped tool run.
5. ~~**Mobile is a port, not a design.**~~ **Fixed.** Designed from its own
   mockup: persistent tab bar, bucket filter chips, ~44px targets, the docked
   decision, and panels as a bottom sheet.
6. ~~**No fleet-level overview.**~~ **Fixed.** The list is full-width and *is*
   the overview: a live census in the header, decisions inline, a dense WORKING
   table with per-session in-flight tool lines, and a collapsed quiet tail.

Character: this is an operator's console, used for hours, mostly dark. It
should read as calm and dense — closer to a trading terminal or a good CI
dashboard than a chat app. But the moment a session needs a human, that
must be impossible to miss.
