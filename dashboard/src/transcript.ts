import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

export type QuestionOption = { label: string; description: string };
export type Question = { question: string; header: string; options: QuestionOption[]; multiSelect?: boolean };

// Visual distinction between "agent" and "human" is now a markdown
// blockquote, not a text label (see Markdown.tsx) — `> ` needs to prefix
// every line, not just the first, for a multi-line human message to render
// as one blockquote instead of one quoted line followed by plain text.
export function asDisplayMarkdown(entry: { from: string; text: string }): string {
  if (entry.from !== "human") return entry.text;
  return entry.text
    .split("\n")
    .map((line) => `> ${line}`)
    .join("\n");
}

// AskUserQuestion (docs/adr/0018) posts `text` as JSON, not prose — these
// parse it defensively so a malformed/future-shaped payload falls back to
// a plain bubble instead of crashing the page.
export function parseQuestions(text: string): Question[] | null {
  try {
    const parsed = JSON.parse(text) as { questions?: unknown };
    return Array.isArray(parsed.questions) ? (parsed.questions as Question[]) : null;
  } catch {
    return null;
  }
}

export function parseAnswers(text: string): Record<string, string> | null {
  try {
    const parsed = JSON.parse(text) as { answers?: unknown };
    return parsed.answers && typeof parsed.answers === "object" ? (parsed.answers as Record<string, string>) : null;
  } catch {
    return null;
  }
}

// Only one question is ever pending per task at a time (the agent's tool
// call blocks on it) — the latest QUESTION entry with no later ANSWER.
export function findPendingQuestion(entries: TranscriptEntry[]): TranscriptEntry | null {
  const idx = entries.findIndex(
    (e, i) =>
      e.type === TranscriptEntryType.QUESTION &&
      !entries.slice(i + 1).some((later) => later.type === TranscriptEntryType.ANSWER),
  );
  return idx >= 0 ? entries[idx] : null;
}

export type PendingPermission = { entry: TranscriptEntry; tool: string; input: unknown };

// A PERMISSION_REQUEST entry is pending until a PERMISSION_RESPONSE entry
// replies to its own seq (real correlation, not the old "any later
// human-originated entry" heuristic ExitPlanMode used to rely on before
// `reply_to` existed on the wire — see docs/adr supersession of 0021/0025),
// OR until a later INTERRUPT/ABORT entry — worker/src/session.ts's
// resolveAllPendingDeny denies every currently-pending permission when
// either arrives, but only in-memory (resolving canUseTool's blocked
// Promise directly); neither posts a real PERMISSION_RESPONSE row, since
// there's no single request it's replying to. This is deliberately
// position-based (later.seq > e.seq), unlike reply_to correlation — sound
// specifically because interrupt/abort really do deny *everything* pending
// at that moment, confirmed live against a real worker (kind-local).
// Multiple can be pending at once now that canUseTool is a generic
// per-tool-call gate rather than an ExitPlanMode-only special case, so this
// returns every unresolved one, not just the latest.
export function findPendingPermissions(entries: TranscriptEntry[]): PendingPermission[] {
  const out: PendingPermission[] = [];
  for (const e of entries) {
    if (e.type !== TranscriptEntryType.PERMISSION_REQUEST) continue;
    let parsed: { tool?: unknown; input?: unknown };
    try {
      parsed = JSON.parse(e.text);
    } catch {
      continue;
    }
    if (typeof parsed.tool !== "string") continue;
    const resolved = entries.some(
      (later) =>
        (later.type === TranscriptEntryType.PERMISSION_RESPONSE && later.replyTo === e.seq) ||
        ((later.type === TranscriptEntryType.INTERRUPT || later.type === TranscriptEntryType.ABORT) && later.seq > e.seq),
    );
    if (!resolved) out.push({ entry: e, tool: parsed.tool, input: parsed.input });
  }
  return out;
}

// Maps a resolved PERMISSION_REQUEST's own seq to what actually happened to
// it, so a collapsed PermissionCard/PlanCard can show the real outcome
// instead of hardcoding "allowed":
// - "allow"/"deny": RespondToPermission's decisionJson ({behavior:...}),
//   mirrored verbatim into the response entry's text.
// - "interrupted": no PERMISSION_RESPONSE exists at all — resolved only via
//   a later INTERRUPT/ABORT, per findPendingPermissions' own reasoning
//   above.
// One pass over entries for the explicit responses, a second bounded by
// only the still-unmapped requests for the implicit case — not a per-card
// rescan either way.
export function resolvedPermissionDecisions(entries: TranscriptEntry[]): Map<bigint, "allow" | "deny" | "interrupted"> {
  const out = new Map<bigint, "allow" | "deny" | "interrupted">();
  for (const e of entries) {
    if (e.type !== TranscriptEntryType.PERMISSION_RESPONSE || e.replyTo === undefined) continue;
    try {
      const decision = JSON.parse(e.text) as { behavior?: unknown };
      if (decision.behavior === "allow" || decision.behavior === "deny") out.set(e.replyTo, decision.behavior);
    } catch {
      // malformed payload — leave unresolved rather than guess
    }
  }
  for (const e of entries) {
    if (e.type !== TranscriptEntryType.PERMISSION_REQUEST || out.has(e.seq)) continue;
    const interruptedLater = entries.some(
      (later) => (later.type === TranscriptEntryType.INTERRUPT || later.type === TranscriptEntryType.ABORT) && later.seq > e.seq,
    );
    if (interruptedLater) out.set(e.seq, "interrupted");
  }
  return out;
}

// No SDK message marks "a turn started generating" — the earliest signal
// is whatever human-originated entry last unblocked the worker's `for
// await` loop (the initial dispatch, a sent message, an answered question,
// a resolved permission), and it stays true until the next RESULT entry
// closes the turn back out to idle. A pending permission/question is its
// own "waiting on you" state, not this one, so those short-circuit false
// even though they're technically after a human-originated entry too.
export function isCogitating(
  entries: TranscriptEntry[],
  podLive: boolean,
  hasPendingPermission: boolean,
  hasPendingQuestion: boolean,
): boolean {
  if (!podLive || hasPendingPermission || hasPendingQuestion) return false;
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (e.type === TranscriptEntryType.RESULT) return false;
    if (
      e.type === TranscriptEntryType.ANSWER ||
      e.type === TranscriptEntryType.PERMISSION_RESPONSE ||
      (e.type === TranscriptEntryType.DISCUSSION && e.from === "human")
    ) {
      return true;
    }
  }
  return false;
}

export type ToolCallSummary = { branch?: string; files?: { path: string; added: number; removed: number }[] };

// The sidecar's periodic telemetry push always sends {branch, files[]}
// (sidecar/internal/telemetry/loop.go), but its local HTTP endpoint
// forwards arbitrary caller-supplied JSON as the same entry type too — so
// nothing here is guaranteed present, hence the defensive parse.
export function parseToolCallSummary(text: string): ToolCallSummary | null {
  try {
    const parsed = JSON.parse(text) as ToolCallSummary;
    return typeof parsed === "object" && parsed !== null ? parsed : null;
  } catch {
    return null;
  }
}

// Latest TOOL_CALL entry's summary — the CHANGES panel only ever shows the
// most recent snapshot, not a running diff across every push.
export function latestToolCallSummary(entries: TranscriptEntry[]): ToolCallSummary | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].type === TranscriptEntryType.TOOL_CALL) {
      return parseToolCallSummary(entries[i].text);
    }
  }
  return null;
}

// The raw Claude Agent SDK message discriminants worker/src/session.ts's
// logSdkMessage relays verbatim (reliability-findings.md #0: "relay
// everything, let the UI decide"). Defensive parse like the helpers above —
// a malformed payload falls back to the raw text bubble instead of crashing.
export type SdkSystemInfo = { model?: string; permissionMode?: string; slashCommands?: string[]; skills?: string[] };
export function parseSdkSystemInfo(text: string): SdkSystemInfo | null {
  try {
    return JSON.parse(text) as SdkSystemInfo;
  } catch {
    return null;
  }
}

// The palette's data source — bare command names only (the SDK's own system/
// init message doesn't carry descriptions/argument hints at runtime), so the
// dashboard's slash-command autocomplete stays a lean name list, not a rich
// command browser. Same "walk backwards to the latest SYSTEM entry" pattern
// as latestToolCallSummary/latestTodos below.
export function latestSlashCommands(entries: TranscriptEntry[]): string[] | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].type === TranscriptEntryType.SYSTEM) {
      const info = parseSdkSystemInfo(entries[i].text);
      return info?.slashCommands && info.slashCommands.length > 0 ? info.slashCommands : null;
    }
  }
  return null;
}

export type SdkToolUse = { id?: string; tool?: string; input?: unknown };
export function parseSdkToolUse(text: string): SdkToolUse | null {
  try {
    return JSON.parse(text) as SdkToolUse;
  } catch {
    return null;
  }
}

export type SdkToolResult = { toolUseId?: string; isError?: boolean; content?: unknown };
export function parseSdkToolResult(text: string): SdkToolResult | null {
  try {
    return JSON.parse(text) as SdkToolResult;
  } catch {
    return null;
  }
}

// Pairs an ASSISTANT (tool_use) entry with its USER (tool_result) entry via
// the Anthropic content-block id<->tool_use_id correlation worker/src/
// session.ts's logSdkMessage now carries — without this, a tool call and
// its output render as two unrelated-looking bubbles with no visible link.
// `result` is null while the call is still in flight (no matching
// tool_result has arrived yet).
export type ToolCallPair = { call: TranscriptEntry; callInfo: SdkToolUse; result: TranscriptEntry | null; resultInfo: SdkToolResult | null };

export function buildToolCallPairs(entries: TranscriptEntry[]): ToolCallPair[] {
  const pairs: ToolCallPair[] = [];
  for (const entry of entries) {
    if (entry.type !== TranscriptEntryType.ASSISTANT) continue;
    const callInfo = parseSdkToolUse(entry.text) ?? {};
    // TodoWrite has its own dedicated TODOS panel (latestTodos below) —
    // showing it again as a generic raw-JSON tool call would just be a
    // worse duplicate of the same data.
    if (callInfo.tool === "TodoWrite") continue;
    const resultEntry = callInfo.id
      ? (entries.find((e) => e.type === TranscriptEntryType.USER && parseSdkToolResult(e.text)?.toolUseId === callInfo.id) ?? null)
      : null;
    pairs.push({ call: entry, callInfo, result: resultEntry, resultInfo: resultEntry ? parseSdkToolResult(resultEntry.text) : null });
  }
  return pairs;
}

// USER entries already folded into a ToolCallPair above shouldn't also
// render as a standalone bubble wherever the feed still shows tool
// output inline (mobile) — an orphaned tool_result (no matching call, e.g.
// truncated history) still falls through and renders on its own.
export function pairedResultSeqs(pairs: ToolCallPair[]): Set<bigint> {
  return new Set(pairs.filter((p) => p.result).map((p) => p.result!.seq));
}

// The TODOS panel's real data source — Claude Code's built-in TodoWrite
// tool (the agent calls it throughout a session to track its own plan) is
// already relayed like any other tool_use via logSdkMessage; nothing new to
// wire up, just read the latest call's full list instead of showing a fake
// one. `null` before the agent's first TodoWrite call.
export type TodoItem = { content: string; status: "pending" | "in_progress" | "completed"; activeForm: string };
export function latestTodos(entries: TranscriptEntry[]): TodoItem[] | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (e.type !== TranscriptEntryType.ASSISTANT) continue;
    const info = parseSdkToolUse(e.text);
    if (info?.tool !== "TodoWrite") continue;
    const todos = (info.input as { todos?: unknown })?.todos;
    return Array.isArray(todos) ? (todos as TodoItem[]) : null;
  }
  return null;
}

// Best-effort one-line preview for a collapsed tool-call header — common
// fields across the built-in tools (Read/Write/Edit/Bash/Grep/Glob), falling
// back to compact JSON for anything else rather than showing nothing.
export function summarizeToolInput(input: unknown): string {
  if (input && typeof input === "object") {
    const obj = input as Record<string, unknown>;
    for (const key of ["file_path", "command", "pattern", "path", "description", "url", "plan"]) {
      const value = obj[key];
      // First line only — matters for "plan" (multi-line markdown), harmless
      // for the single-line fields. Without this, ToolCallItem's shortValue
      // (which finds the last "/" for path-like values) can end up taking
      // an unrelated tail from deep inside a multi-line value instead of an
      // actual preview.
      if (typeof value === "string") return value.split("\n")[0];
    }
  }
  try {
    return JSON.stringify(input);
  } catch {
    return String(input);
  }
}

export type SdkResultSummary = { subtype?: string; numTurns?: number; totalCostUsd?: number };
export function parseSdkResultSummary(text: string): SdkResultSummary | null {
  try {
    return JSON.parse(text) as SdkResultSummary;
  } catch {
    return null;
  }
}
