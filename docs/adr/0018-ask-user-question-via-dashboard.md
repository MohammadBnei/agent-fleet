# ADR-0018: AskUserQuestion is real, answered via the dashboard, not Discord

**Status:** Accepted
**Date:** 2026-08-04

## Context

`docs/adr/0017` gave the planner a "Discord-as-interactive-user"
reinterpretation for `architecture-interview`/`doubt-driven-development`'s
interview steps: since a real `AskUserQuestion` tool doesn't exist in a
headless Agent SDK session, the planner's prompt told it to never attempt
that call and to route interview questions through `send_message`/
`wait_for_messages` to Discord instead — one question, one plain-text
reply, repeat.

This works but is a poor fit for what `AskUserQuestion` actually is: a
*structured* multiple-choice tool (a header, several options each with a
label and description, optional multi-select). Discord threads render
plain text only — a structured question degrades to a wall of text the
human has to parse and reply to freehand, re-typing an option label
exactly, with no UI affordance at all. The dashboard (`docs/adr/0014`,
`docs/adr/0015`) already exists, already streams the live transcript, and
is a far better fit for rendering an actual form.

## Decision

**A real `AskUserQuestion` MCP tool now exists**, registered in
`fleet-core/internal/mcpserver/server.go` alongside `send_message`/
`wait_for_messages`. Its input schema is reflected from a Go struct
(`AskUserQuestionArgs` in `ask_user_question.go`, via `mcp.WithInputSchema`)
matching Claude Code's native tool shape — `questions: [{question, header,
options: [{label, description}], multiSelect}]` — so a skill's own
instructions ("use AskUserQuestion") produce a well-formed call without any
prompt-level translation.

**Answered via the dashboard, never Discord.** The tool appends a
`type: "question"` entry to `planning_transcript` (`text` is a JSON
payload, not prose) and long-polls (same bounded-timeout-per-call shape as
`wait_for_messages`, default 60s, returning `{"status":"pending"}` to
retry rather than blocking one HTTP call indefinitely) for a matching
`type: "answer"` entry. The dashboard's `TaskDetail` page
(`dashboard/src/pages/TaskDetail.tsx`) renders `QUESTION` entries as an
actual form (radio/checkbox per option) and submits via a new
`AnswerQuestion` RPC (`fleet-core/internal/dashboard/server.go`) — the
exact same `transcr.Append(..., "human", ..., "answer", ...)` shape
`Approve`/`Stop` already use, no new store or business logic
(`docs/adr/0014`'s reuse-existing-stores rule holds).

**No correlation ID.** Only one `AskUserQuestion` call is ever outstanding
per task — the planner's own tool call blocks synchronously before it does
anything else — so "the next `answer` entry after this `question` entry's
own seq" is an unambiguous match, the same assumption `isApproval`/
`isAbort` already make elsewhere in this codebase.

**Discord still owns everything else.** Narrative plan text, doubt-cycle
status, and the `/approve`/`/stop` approval gate (ADR-0005, unchanged) stay
on `send_message`/`wait_for_messages` — only *structured* questions moved.
`watchBatch` (`worker/src/planning.ts`) relays a `question`-type entry to
Discord as a one-line pointer ("asked a question — answer it on the
dashboard") instead of dumping the raw JSON payload, and explicitly skips
`answer`-type entries in its `isApproval`/`isAbort` checks — a human's
chosen option label could otherwise false-positive-match the word-matching
approval/abort fallback (e.g. an option literally labeled "approved" would
have short-circuited the whole planning phase).

**This partially supersedes `docs/adr/0017`.** Only its
Discord-as-interactive-user section — everything else in that ADR (single
planner session, `PLAN_READY:` round-cap, doubt-driven's `Task`-tool
subagent, no `/task` override knob) is unaffected and still holds.

## Consequences

- `planning_transcript.type`'s CHECK constraint gains `'question'`/
  `'answer'` (`db/schema.sql`); `TranscriptEntryType` proto enum gains
  `QUESTION`/`ANSWER` (`proto/agentfleet/v1/transcript.proto`).
- Interview questions now require the dashboard to be open and reachable —
  a task that only relies on Discord will have its interview steps stall
  (bounded to repeated `{"status":"pending"}` retries, not an unbounded
  hang, but effectively blocked until someone visits the dashboard). This
  is an accepted tradeoff: structured UI needs a structured client.
  `doubt-driven-development`'s Task-tool subagent path (no human
  interaction needed) is unaffected.
- One thing to verify live once deployed: whether the planner reliably
  calls the real `AskUserQuestion` tool now that it's actually available
  (rather than defaulting to the old send_message/wait_for_messages
  pattern out of habit from how the prompt used to read) — this replaces,
  not adds to, `docs/adr/0017`'s second open risk (the model reaching for
  a *nonexistent* tool). Check `tool_use` entries naming `AskUserQuestion`
  in `kubectl logs` against a real interview-triggering task.
