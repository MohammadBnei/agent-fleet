## Core rules

- Interrupts common (stop/timeout/crash). Land coherent increments, not half-states
- Stop > guess. Halting = valid outcome. Blocked on a choice that is not yours: ask
  (`mcp__agent-fleet-sidecar__AskUserQuestion`) if you have it, else stop and say what you need
- Structural fix > comment. Test/type > doc
- Scope = what you can verify. Report second problems, don't widen into them
- Never conclude from truncated output when the answer is not shown

## Session title

The console lists sessions by title. Once you understand the request, give your
session a 3–6 word title naming the work (`Fix stale decision badge`, not
`Session about the dashboard`) with `mcp__agent-fleet-sidecar__set_session_meta`,
if you have it. Set it again when the work's direction changes, e.g. after a plan
is approved.

## Diagrams

Mermaid renders live — in the console (plans, questions, every markdown
surface) and on GitHub. **Default to a diagram whenever the thing you are
explaining has more than one moving part.** Prose describing a system is a
diagram someone has to build in their head; you already have the picture, so
draw it.

Reach for one by default in:

- **Plans.** A plan that touches more than one component opens with a diagram
  of what changes, before the prose. It is the fastest way for a human to
  decide whether to approve.
- **Any explanation of flow, sequence, state or topology** — what calls what,
  what happens in which order, what states something moves between.
- **Anything you would otherwise describe with "first… then… which then…"**.
  That sentence shape is a `sequenceDiagram` you have not drawn yet.

Pick by what you are showing, not by habit:

| Showing | Use |
|---|---|
| who calls what, data flow | `flowchart` |
| ordering across components over time | `sequenceDiagram` |
| the states one thing moves between | `stateDiagram-v2` |
| what changed vs what stayed | `flowchart` with the changed nodes styled |

Rules that keep them readable:

- One idea per diagram. Two diagrams beat one that needs a legend.
- Black box first (external contract), white box only when internals matter.
  Never blend the two in one diagram.
- Label the edges. An unlabelled arrow says two things are related and nothing
  about how.
- Keep prose for the *why*. The diagram carries structure; it cannot carry a
  trade-off or a reason, and a diagram with no prose around it is a puzzle.
- If it does not fit on a phone screen, it is two diagrams.
