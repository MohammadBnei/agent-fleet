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
| 4 · Propose, never author *(initiation)* | Machine-created work lands in `proposals`, a separate table with no path to a pod. Sessions are created only by an authenticated principal: an admin in the OIDC-gated console, a definition's audience member, or a registered client on `AgentService` (`:9091`, `adr/0066`, `adr/0062`). Nothing self-initiated runs until a human opens the proposal (`OpenFromProposal`, `adr/0048`). Once a session exists, another session may prompt and warm it (`prompt_agent`, `adr/0041`). Today a non-admin audience exists (`opus-workspace`, group `shared-notes`), and no client is registered to open anything (`bridge` lists no definitions) | **Real — structural** |
| 3 · The gap is the work | Scheduled cluster checks and firing alerts become deduped proposals against `infra-bootstrap`, so a durable fix is an ordinary PR (`adr/0035`, `adr/0037`) | **Real** |
| 6 · Apoptosis over degradation | Single-shot pods, `RestartPolicy: Never`, crash-fast path (`adr/0024`) | **Real** |
| 7 · Structural before asked | `buildguard`, the LimitRange↔pod-spec pin, migrations as the single schema source (`adr/0030`) | **Real** |
| 8 · Reversibility | One volume per session, never shared writable (`adr/0048`). A PR is the intended result, but no code checks that one was opened. In `auto` — the default (migration 000007) — a repo session's `git push` and `gh` are answered without a prompt (only `rm`, `sudo` and a plan still ask, `worker/src/session.ts`), and its App token can write contents and PRs (never `workflows`). So "nothing merges itself" rests on branch rulesets: `infra-bootstrap` has `protect-main`; `agent-fleet` and `ukubi-bridge` are getting theirs. A definition with `git.mode: push` pushes straight to its branch — the declared exception, shipped with `adr/0065` | **Structural once the rulesets are live** |
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

**Principle 4 is split, and the halves are in different tiers.** Nothing
machine-initiated can *execute* without approval — that half is enforced by
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

## The frontier

Same as the parent's, and it applies most sharply here because this is the
part that could act: **today the fleet converges, it does not originate.**
Every session begins with a human. An agent may propose, never author.

The stated next frontier is **origination with human selection**: agents may
propose new work, not only repairs. Sources, in order: drift and alerts;
usage and observability; repo activity; cross-project synergy (hq's map). A
human selects every proposal. Agents never merge.

It is not taken until the convergent loop above is running and trusted.
