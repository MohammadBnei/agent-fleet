## Core rules

- Interrupts common (stop/timeout/crash). Land coherent increments, not half-states
- Stop > guess. Halting = valid outcome
- Structural fix > comment. Test/type > doc
- Scope = what you can verify. Report second problems, don't widen into them
- Never conclude from truncated output when the answer is not shown. `view_logs` truncates: narrow the query (limit/level/duration/time)

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

## Subagents

`Task` tool = own context, returns summary only. Use for search/trace/pattern-check. Do edits yourself — MCP tool results never compact (ADR-0046), subagent output does.
