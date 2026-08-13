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
source from GitHub, images from Docker Hub, identity from an external bot
account. It works, and it is not yet self-contained.

**The metaphor is literal here, not analogical.** Elsewhere "cells" is a
figure of speech. In this repo it is the actual design:

- A **worker pod is a cell.** Single-shot, ephemeral, spawned on demand,
  dying by design (`docs/adr/0029`). It carries the full context and
  expresses the part relevant to its position.
- A **session is the tissue** that persists while its cells turn over — the
  durable unit, resumable across pods, which is precisely why `docs/adr/0029`
  moved the durable unit from task to session.
- The **worktree is the cell membrane.** One per task, never shared. What is
  inside is yours; what is outside is not yours to touch.
- **`knowledge_journal` is the memory** that outlives any individual cell.

That is why the rules land here first. This is where they stop being a
description and become behaviour.

## What is already true

Honest audit, not aspiration. `docs/ARCHITECTURE.md` is authoritative for
detail; this is only the mapping.

| Principle | Mechanism here | State |
|---|---|---|
| 4 · Propose, never author *(initiation)* | Machine-created tasks land in `proposed`, never `pending`, and dispatch only ever reads `pending` — so nothing self-initiated can run until `ApproveProposal` flips it (`adr/0037`) | **Real — structural** |
| 3 · The gap is the work | The `audits` loop turns scheduled cluster checks and firing alerts into deduped tasks against `infra-bootstrap`, so a durable fix is an ordinary PR (`adr/0035`, `adr/0037`) | **Real** |
| 6 · Apoptosis over degradation | Single-shot pods, `RestartPolicy: Never`, crash-fast path (`adr/0024`) | **Real** |
| 7 · Structural before asked | `buildguard`, the LimitRange↔pod-spec pin, migrations as the single schema source (`adr/0030`) | **Real** |
| 8 · Reversibility | Every result is a PR; one worktree per task; nothing merges itself | **Real** |
| 9 · It remembers | `knowledge_journal` + `journal_search` (`adr/0033`) | **Real** |
| 2 · Declared intent is truth | The docs are canonical and alerts feed the audit loop, but nothing yet diffs *declared intent* against reality — `mission-drift` lives in the parent and is report-only | **Partial** |
| 5 · Staged, checkpointed repair | Worktree isolation and PR-per-result give the shape; nothing enforces that an *interrupted* run leaves a valid state, and interruption is the common case | **Partial** |
| 4 · Genome authored *(spec edits)* | Nothing stops an agent editing a spec to make its own work compliant. Only a rule in `fleet-shared/CLAUDE.md` — the weakest tier | **Rule only** |
| 1 · Self-contained substrate | GitHub, Docker Hub, external identity | **Gap** |

Two things worth noticing.

**The loop is already closed once.** A firing alert becomes a proposed task
against the repo that holds the cluster's own IaC, which a human approves,
which a cell then works as an ordinary PR. That is perception → proposal →
human selection → action, running today. The organism already notices
something is wrong with itself and asks to fix it.

**Principle 4 is split, and the halves are in different tiers.** Nothing
machine-initiated can *execute* without approval — that half is enforced by
a status column dispatch doesn't query, which is why it holds. But an agent
editing `ARCHITECTURE.md` mid-task to make its own work look compliant is
prevented only by having been asked nicely. Same principle, opposite ends of
principle 7's ladder — and the difference between them is exactly the
argument for moving rules down it.

## Where it goes next

Direction only — each becomes an ADR when actually taken.

- **Widen the loop's input, not its authority.** The alert→proposal path
  works; what feeds it is narrow. Declared-intent drift (`mission-drift`, in
  the parent) is the obvious second source, and it needs no new authority —
  it lands in the same `proposed` queue behind the same human.
- **Move principle 4's other half down a tier.** "Don't edit the genome" should be
  detectable, not merely requested — a check that a PR touching
  `ARCHITECTURE.md`/`DECISIONS.md`/`docs/adr/` is flagged as a spec change
  rather than passing as ordinary work.
- **Make "valid resting state" mean something.** A worker is interrupted by
  stop, idle timeout, or crash far more often than it finishes cleanly.
  What it leaves behind on those paths is currently untested.
- **Cut the umbilical** when the substrate is ready: in-cluster registry
  first, then mirrored git (parent VISION, "what this implies next").

## The frontier

Same as the parent's, and it applies most sharply here because this is the
part that could act: **the fleet converges, it does not originate.** Every
task begins with a human. An agent may propose, never author.

Whether a cell ever gets to originate rather than express is the open
question. It is not answered here, and it should not be answered until the
convergent loop above is running and trusted.
