---
name: architecture-interview
description: Interviews the user about a software architecture decision — new system/feature design or review of an existing architecture — one question at a time, probing quality-attribute trade-offs, constraints, alternatives, and reversibility, until confident enough to write an ADR. Use when the user explicitly invokes ("interview me about this architecture", "architecture interview", "stress-test this design"), asks for review or validation of an existing design ("review my architecture", "is this the right design", "did we make the right call here"), or requests a new system/feature/component design without a stated quality-attribute priority, constraint, or alternative considered.
---

# Architecture Interview

## Overview

An architecture decision made without naming which quality attribute wins under conflict, what alternatives were rejected and why, and how expensive it is to reverse, tends to get re-litigated later or silently regretted. The cheapest moment to surface those trade-offs is before the decision is written down or code commits to it — not after, when switching costs are real and the decision gets rationalized into "good enough" instead of examined.

`interview-me` closes the intent gap (who/why/success/constraint). This skill assumes intent is already known and interrogates the *design* that serves it: which quality attribute wins when two are in tension, what was considered and rejected, what happens when it breaks, and whether the decision is a one-way or two-way door. Its output is shaped to drop directly into an ADR (`documentation-and-adrs`).

## When to Use

- A new system, service, or component is being designed and quality-attribute priorities, constraints, or alternatives haven't been stated
- The user asks for a review or validation of an *already-built* architecture ("review my architecture", "is this the right design", "did we make the right call")
- A decision is being made that's expensive to reverse: schema shape, wire protocol, sync boundary, vendor/framework lock-in, public API shape
- Two quality attributes are in tension and the user hasn't said which wins (consistency vs. availability, latency vs. cost, simplicity vs. flexibility, build vs. buy)
- Explicit invocation: "interview me about this architecture", "architecture interview", "stress-test this design"

**When NOT to use:**

- The decision is genuinely reversible and low blast radius (internal-only refactor, naming, a config value) — just make the call
- The decision is already made and the reasoning is already known — skip straight to `documentation-and-adrs` to write it down
- The general intent or purpose of the work isn't established yet (who/why/success) — run `interview-me` first; this skill starts from confirmed intent
- Pure information requests ("how does our current architecture handle X?", "explain this diagram")

## Loading Constraints

This skill needs a live, responsive user. **Do not invoke in non-interactive contexts** (CI, scheduled runs, `/loop`, autonomous-loop). If a design decision is underspecified there, flag it as a blocker instead of guessing.

## The Process

### Step 1: Name the decision and its reversibility

Before asking anything, state the decision in one sentence and classify it:

```
DECISION: <one sentence — what's actually being decided>
DOOR:     one-way | two-way — <why>
```

One-way doors (schema choices, protocol changes, vendor lock-in, public API shape) warrant the full interview below. Two-way doors with genuinely low blast radius are a signal to stop short — say so and make the call (see "When NOT to use"). Don't downgrade rigor just because the *code change* looks small; check who or what actually depends on the decision first.

### Step 2: Hypothesize the quality-attribute priority, with confidence

Same discipline as `interview-me`: one-sentence hypothesis plus an honest 0–100% confidence number, naming what's still unresolved below ~70%.

```
HYPOTHESIS: You're optimizing for write latency over strict consistency — the access pattern you described is high-frequency, single-writer.
CONFIDENCE: ~40% — missing: read-consistency requirements, and whether this data ever needs cross-region access
```

### Step 3: One question at a time, each with a guess attached

Same mechanics as `interview-me` — one question per turn, your hypothesis attached, wait for a reaction before the next one. Draw questions from whichever of these categories has the lowest confidence; don't ask all of them:

- **Quality-attribute conflict** — which wins when X and Y trade off (consistency/availability, latency/cost, simplicity/flexibility, build/buy)
- **Scale** — current load and expected growth as an actual number or order of magnitude, not "web scale"
- **Constraints** — team size/expertise, existing infra it must fit into, deadline, budget
- **Failure mode** — what happens when this breaks, who or what is affected, how it's detected
- **Alternatives** — what else was considered and why it lost. Probe even if the user hasn't mentioned any; anchoring on the first idea is the most common architecture mistake
- **Boundary** — what this decision explicitly does *not* cover, or defers

Format:

```
Q:     <one focused question>
GUESS: <your hypothesis for the answer, with the reasoning behind it>
```

### Step 4: Probe stated-vs-actual priority

Watch for buzzwords doing the work of a requirement: "scalable", "clean architecture", "microservices", "event-driven", "future-proof" offered as the goal itself rather than justified by a concrete scenario. When you hear one:

> *"What breaks if you don't do this — what's the actual scenario, not the principle?"*

### Step 5: Restate as a decision summary (ADR-shaped)

When confidence is high, restate in a form that maps directly onto an ADR:

```
Here's the decision as I now understand it:

- Decision:                  <one line — what we're doing>
- Context/Problem:           <one line — what forced this>
- Quality attribute priority: <what wins under conflict>
- Constraints:                <one line — binding limits: team, infra, deadline>
- Alternatives considered:    <bullet list — option: why rejected>
- Consequences:               <one line — what we're accepting or giving up>
- Out of scope / deferred:    <one line>
- Reversibility:              one-way door | two-way door

Yes / no / refine?
```

"Alternatives considered" needs more than one entry (the chosen option alone isn't alternatives — see Red Flags). "Out of scope" is non-negotiable, same reasoning as `interview-me`: silent disagreement about what's *not* being built is half of misalignment.

### Step 6: Confirm — explicit yes, not "whatever you think"

Identical gate to `interview-me`: "whatever you think," "sounds good," "sure, let's go," and silence-then-"okay let's start" are not confirmations — each gets a specific re-ask. Loop on corrections until an explicit yes.

### The Stop Condition

You're done when you can answer yes to:

> *Could I write this decision's ADR right now — including a real "Alternatives Considered" section — without guessing?*

If yes, stop and restate (Step 5). If no, keep asking. Same floor as `interview-me`: several rounds without confidence rising is information about the decision, not a reason to keep grinding — stop and say so.

## Output

A confirmed architecture-decision summary (Step 5 restate + explicit yes). That's the deliverable — not an ADR file itself. Offer to hand off to `documentation-and-adrs` to write the actual ADR (it will check the project's existing location/numbering convention); only do so if the user confirms.

## Example

**Without this skill:**

```
User:  I want to switch this endpoint from REST to GraphQL
Agent: <starts scaffolding a GraphQL schema and resolvers>
```

**With this skill:**

```
User: I want to switch this endpoint from REST to GraphQL

Agent:
DECISION: Replace the REST endpoint with a GraphQL endpoint for this resource.
DOOR:     one-way — protocol change touches every client of this endpoint.

HYPOTHESIS: You're optimizing for flexible client queries over the caching simplicity REST gives you for free.
CONFIDENCE: ~30% — missing: what's actually painful about the REST version right now.

Q:     What's the concrete problem you're hitting — is it over-fetching (clients get fields they don't need), under-fetching (clients make several round trips), or something else?
GUESS: Under-fetching — a client-heavy dashboard page probably needs data from 3-4 REST calls today.

User: Actually it's one specific page that over-fetches a huge nested object it only needs two fields from.

Agent:
Updated read: this isn't a protocol-wide decision at all — it's one page with one over-fetching call.
Confidence: ~75% — still missing: whether other clients hit the same endpoint and would also need the trim.
```

Two questions in, "switch to GraphQL" became "add one trimmed-down endpoint or field selection for one page" — a two-way door, much smaller than a protocol migration, and cheap to reverse if wrong.

## Interaction with Other Skills

- **`interview-me`**: upstream. If the general intent/purpose isn't established yet, run that first; this skill starts from confirmed intent and interrogates the design serving it.
- **`documentation-and-adrs`**: downstream. The confirmed decision (Step 5) is handed off to fill its ADR template directly — field for field.
- **`api-and-interface-design`**: consulted mid-interview when the decision is specifically about an API/interface boundary; draw probes from its principles (contract-first, Hyrum's Law, versioning) rather than reinventing them.
- **`spec-driven-development`**: sibling/downstream. If the confirmed decision is one piece of a larger feature, it feeds into that feature's spec rather than standing alone.
- **`doubt-driven-development`**: opposite end of the timeline, same pairing as `interview-me`/`doubt-driven-development` — this is pre-decision interrogation, doubt-driven is post-decision adversarial review.

## Common Rationalizations

| Rationalization | Reality |
|---|---|
| "It's obviously the scalable choice" | Scalable for what load? Would you notice if you'd guessed wrong for two years? Run Step 2. |
| "There's no alternative, this is the only way" | There's always at least "do nothing" and "the simplest possible version." Probe both before accepting "no alternatives." |
| "It's just how we've always done it" | That's a constraint (team familiarity, existing infra), not a quality-attribute justification. Name it as a constraint explicitly. |
| "This is a two-way door, no need to interview" | Confirm the actual blast radius before downgrading rigor — check who else depends on it, not just how easy the code change looks. |
| "I'll document alternatives after I decide" | Alternatives considered *before* deciding surface the real trade-off; documented after, they get rationalized to fit the decision already made. |

## Red Flags

- Three or more questions in a single message — batching, not interviewing
- A question without your hypothesis attached
- Accepting a buzzword ("scalable", "clean", "future-proof") as the stated quality attribute without probing for the concrete scenario behind it
- An "Alternatives considered" list with only the chosen option — that's not alternatives, that's a decision looking for cover
- Classifying a decision "two-way door" without checking who or what actually depends on it
- Producing a plan, ADR, or implementation before the user has explicitly confirmed the Step 5 restate
- Skipping the "Out of scope / deferred" line

## Verification

- [ ] Decision named and classified (one-way/two-way door) in the first turn
- [ ] An explicit hypothesis with a confidence number was stated before the first question
- [ ] Every confidence number below ~70% had a one-line reason attached
- [ ] Questions were asked one at a time, each with a guess attached
- [ ] At least one "what breaks if you don't do this" probe ran when the user gave a buzzword answer
- [ ] The restate included a real Alternatives Considered list (more than just the chosen option) and an Out of scope line
- [ ] The user confirmed with an explicit yes, not a vague agreement
- [ ] At the stop point, the agent could write the ADR's Alternatives Considered section without guessing
