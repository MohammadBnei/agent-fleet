# VISION — the fleet's place in the organism

**Parent:** [`infra-bootstrap/VISION.md`](https://github.com/MohammadBnei/infra-bootstrap/blob/main/VISION.md)
is the genome. This file does not restate it — it says what agent-fleet
specifically is within it, and which of its principles are already real here
versus still aspirational.

As always: `docs/ARCHITECTURE.md` wins for topology, `docs/DECISIONS.md` and
`docs/adr/` win for decisions. A vision does not override a spec.

---

## What this repo is

agent-fleet is the first organ to actually *live* in the organism — the
first thing in the cluster that perceives, acts, and remembers rather than
merely running. Everything else deployed on `ukubi-cluster` is tissue that
holds shape. This is the part with a metabolism.

It is also, for now, a foetus in an artificial womb. It is gestating inside
a substrate that is still being built around it, on borrowed circulation:
source from GitHub, base images from Docker Hub, identity from a GitHub App
(`ukubi-fleet`, `docs/adr/0075`). It works, and it is not yet self-contained.

**The metaphor is literal here, not analogical.** Elsewhere "cells" is a
figure of speech. In this repo it is the actual design:

- A **worker pod is a cell.** Single-shot, ephemeral, spawned on demand,
  dying by design (`docs/adr/0029`). It carries the full context and
  expresses the part relevant to its position.
- A **session is the tissue** that persists while its cells turn over — the
  durable unit, resumable across pods, which is precisely why `docs/adr/0029`
  moved the durable unit from task to session.
- The **session's own volume is the cell membrane.** One per session, never
  shared writable (`docs/adr/0048`). What is inside is yours; what is outside
  is not yours to touch.
- **`knowledge_journal` is the memory** that outlives any individual cell.

That is why the rules land here first. This is where they stop being a
description and become behaviour.

## What is already true

Honest audit, not aspiration. `docs/ARCHITECTURE.md` is authoritative for
detail; this is only the mapping.

| Principle | Mechanism here | State |
|---|---|---|
| 4 · Propose, never author *(initiation)* | Sessions open from three sources. (1) A principal: an admin in the OIDC-gated console, a definition's audience member, or a registered client on `AgentService` (`:9091`, `adr/0066`, `adr/0062`). (2) An event: the bridge dispatcher (`adr/0071`, `core/internal/dispatch`) wakes a definition's thread for a trusted human's event; anything else lands as a `source='event'` proposal. (3) Another session: `prompt_agent` (`adr/0041`) and `publish_event` (depth 3, a per-session budget, at most 3 agent wakes per thread per 24h). Untrusted input still has no path to a pod except through a proposal someone opens (`OpenFromProposal`, `adr/0048`). Today the `bridge` client lists no definition, so no event opens anything yet | **Real — semi-autonomous by design** |
| 3 · The gap is the work | Scheduled cluster checks and firing alerts become deduped proposals against `infra-bootstrap`, so a durable fix is an ordinary PR (`adr/0035`, `adr/0037`) | **Real** |
| 6 · Apoptosis over degradation | Single-shot pods, `RestartPolicy: Never`, crash-fast path (`adr/0024`) | **Real** |
| 7 · Structural before asked | `buildguard`, the LimitRange↔pod-spec pin, migrations as the single schema source (`adr/0030`) | **Real** |
| 8 · Reversibility | One volume per session, never shared writable (`adr/0048`). A PR is the intended result, but no code checks that one was opened. In `auto` — the default (migration 000007) — a repo session's `git push` and `gh` are answered without a prompt (only `rm`, `sudo` and a plan still ask, `worker/src/session.ts`), and its App token can write contents and PRs (never `workflows`). Merge follows code review in the delivery process (below), so an agent may merge. What makes that safe is meant to be structural: rulesets that require the process's own checks. Today `infra-bootstrap`'s `protect-main` still requires a human review; `agent-fleet` and `ukubi-bridge` have no PR rule yet, so merge there rests on the agent following the process. A definition with `git.mode: push` pushes straight to its branch — the declared exception, shipped with `adr/0065` | **Partial — structural once the process checks are required** |
| 9 · It remembers | `knowledge_journal` + `journal_search` (`adr/0033`) | **Real** |
| 2 · Declared intent is truth | The docs are canonical and alerts feed the audit loop, but nothing yet diffs *declared intent* against reality — `mission-drift` lives in the parent and is report-only | **Partial** |
| 5 · Staged, checkpointed repair | Per-session volumes and PRs give the shape; nothing enforces that an *interrupted* run leaves a valid state, and interruption is the common case | **Partial** |
| 4 · Genome authored *(spec edits)* | Nothing stops an agent editing a spec to make its own work compliant. Only a rule in the `packs/repo` context pack (and `fleet-shared/CLAUDE.md`) — the weakest tier | **Rule only** |
| 1 · Self-contained substrate | Fleet images build and live in-cluster (Zot); source on GitHub, base images from Docker Hub, identity a GitHub App (`ukubi-fleet`) | **Partial** |
| — · Known gap: the build box | Every repo's CI builds as root on one shared build-runner box — PRs and unprotected app repos included. Accepted for now (2026-10-22) | **Accepted gap** |

Two things worth noticing.

**The loop is already closed once.** A firing alert becomes a proposal
against the repo that holds the cluster's own IaC, which a human opens,
which a cell then works as an ordinary PR. That is perception → proposal →
human selection → action, running today. The organism already notices
something is wrong with itself and asks to fix it.

**Principle 4 is split, and the halves are in different tiers.** Untrusted
machine input cannot *execute* until someone opens it — that half is enforced by
proposals living in a table that has no path to a pod, which is why it holds. But an agent
editing `ARCHITECTURE.md` mid-task to make its own work look compliant is
prevented only by having been asked nicely. Same principle, opposite ends of
principle 7's ladder — and the difference between them is exactly the
argument for moving rules down it.

## Where it goes next

Direction only — each becomes an ADR when actually taken. The owner's order
(2026-10-22): converge first, then an agent per service, then originate.

- **Converge first: widen the loop's input, not its authority.** Alerts and
  declared-intent drift both become proposals. The first feature is
  mission-drift → proposals (`mission-drift`, in the parent); it needs no new
  authority — it lands in the same `proposals` table behind the same human.
- **Then an agent per service or app, staged.** A steward per repo first —
  agent-fleet itself is the pilot. Then calls embedded in apps through
  `AgentService`. Resident agents last.
- **Move principle 4's other half down a tier.** "Don't edit the genome" should be
  detectable, not merely requested — a check that a PR touching
  `ARCHITECTURE.md`/`DECISIONS.md`/`docs/adr/` is flagged as a spec change
  rather than passing as ordinary work.
- **Make "valid resting state" mean something.** A worker is interrupted by
  stop, idle timeout, or crash far more often than it finishes cleanly.
  What it leaves behind on those paths is currently untested.
- **Cut the umbilical** when the substrate is ready: the in-cluster registry
  is done; base images and mirrored git are next (parent VISION, "what this
  implies next").

## The process is the quality gate

The owner's delivery process (2026-10-24), as laptop sessions run it today:
ideation → design → interview rounds → (doubt) → architect interview rounds →
(doubt) → plan → doubt → (sync with other sessions) → implementation
(parallel) → quality gates (code quality, structure, design patterns) → live
QA as close to prod as possible → PR → code review → merge → watch deploy →
light QA.

The gate is the stages, not a person standing at one of them. A human comes in
where the process asks for one: interview rounds and the doubts a cycle
escalates. Merge follows code review in the process. Today only CI's own stages
(lint, test, build) leave a check a ruleset can require; the stages before
the PR and the code review leave none yet. Making each stage leave checkable
evidence, and then requiring it, is how principle 8 becomes structural.

## The frontier

The fleet is **semi-autonomous**. Sessions start and wake from events and from
each other's hand-offs, not only from a human. How much a human gates is not
a declared rule. It is a consequence of the system: trust per actor and per
event, budgets (depth, hops, per-thread caps), grants that widen or narrow per
session, and which definitions a bridge client lists.

The frontier the owner holds (2026-10-24) is **originate, human selects**.
Agents may propose new work, not only repairs. Sources, in order: drift and
alerts; usage and observability; repo activity; cross-project synergy (hq's
map). New intent is selected by a human. Work already selected runs through
the process above, merge included.

Origination is not taken until the convergent loop above is running and trusted.
