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

// Only one question is ever pending per session at a time (the agent's tool
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
// either arrives, and for those two it posts no PERMISSION_RESPONSE row,
// since there's no single request it's replying to. (Its *other* caller —
// a plain human reply, which resolves the same way — does post one now, so
// that case needs no positional rule; see recordResolution. Before it did,
// requesting changes on a plan left the request pending forever here.)
// This is deliberately
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

// A decision the human has made but the server hasn't echoed back yet.
//
// Every surface that shows a decision — the feed card, the dock, the list
// row — derives its state from `entries` alone, so the honest way to make a
// click land immediately is to put the answer *in* entries and let all
// three agree, rather than teaching each one about a separate "pending"
// flag. Answering a permission is two round trips before anything visibly
// changes (respondToPermission, then the response entry arriving on the
// transcript stream), which is the window where a card sat there still
// offering allow/deny as though the click never happened.
//
// An optimistic entry is dropped the moment the real one supersedes it, so
// the server always wins: this only ever fills the gap, it never invents a
// decision the fleet didn't record.
export function withOptimistic(real: TranscriptEntry[], optimistic: TranscriptEntry[]): TranscriptEntry[] {
  if (optimistic.length === 0) return real;
  const kept = optimistic.filter((o) => {
    const answers = o.replyTo;
    if (answers === undefined) return false;
    return !real.some(
      (e) =>
        // core appends both kinds via AppendReply, so the real entry carries
        // the same reply_to this one was built with.
        (e.type === o.type && e.replyTo === answers) ||
        // ...or the agent stopped waiting for the decision altogether, which
        // resolves it just as firmly as an answer does.
        ((e.type === TranscriptEntryType.INTERRUPT || e.type === TranscriptEntryType.ABORT) && e.seq > answers),
    );
  });
  return kept.length === 0 ? real : [...real, ...kept];
}

// The human's stated reason for a denial, keyed by the request's own seq.
// Parsed alongside the decision above but kept separate so that Map's type
// stays a plain outcome — only the collapsed card needs this.
export function permissionDenyMessages(entries: TranscriptEntry[]): Map<bigint, string> {
  const out = new Map<bigint, string>();
  for (const e of entries) {
    if (e.type !== TranscriptEntryType.PERMISSION_RESPONSE || e.replyTo === undefined) continue;
    try {
      const decision = JSON.parse(e.text) as { behavior?: unknown; message?: unknown };
      if (decision.behavior === "deny" && typeof decision.message === "string" && decision.message.trim()) {
        out.set(e.replyTo, decision.message);
      }
    } catch {
      // malformed payload — no reason to show
    }
  }
  return out;
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
export type SdkSystemInfo = {
  model?: string;
  permissionMode?: string;
  slashCommands?: string[];
  skills?: string[];
  tools?: string[];
  mcpServers?: { name: string; status: string }[];
  cwd?: string;
  claudeCodeVersion?: string;
  agents?: string[];
  plugins?: { name: string; path: string }[];
  outputStyle?: string;
};

// A SYSTEM entry is one of two things. Without an `sdk` key it is the
// session-init environment (the payload above). With one it is an
// out-of-band signal — every non-init system subtype, plus auth_status and
// tool_progress, which worker/src/session.ts relays under the same entry
// type rather than minting new ones (no migration, and core's Discord
// allowlist keeps them off Discord by failing closed).
//
// Everything reading SYSTEM entries must therefore check `sdk` FIRST.
// Anything that just walks back to "the latest SYSTEM entry" would now
// almost always land on a tool_progress instead of the init payload.
export type SdkSignal = {
  sdk: string;
  status?: string | null;
  compact_metadata?: { trigger?: string; pre_tokens?: number };
  hook_name?: string;
  hook_event?: string;
  exit_code?: number;
  stdout?: string;
  stderr?: string;
  isAuthenticating?: boolean;
  error?: string;
  tool_name?: string;
  tool_use_id?: string;
  elapsed_time_seconds?: number;
  parent_tool_use_id?: string | null;
  // The subagent family (task_started / task_progress / task_updated /
  // task_notification). `tool_use_id` above is what correlates all four back
  // to the originating Agent tool call; `task_id` is the SDK's own handle and
  // is the ONLY key task_updated carries, hence the two-way index in
  // subagentRuns below.
  task_id?: string;
  description?: string;
  subagent_type?: string;
  last_tool_name?: string;
  summary?: string;
  usage?: { total_tokens?: number; tool_uses?: number; duration_ms?: number };
  patch?: { status?: string; end_time?: number };
  // code_change_published / vcs_state_changed / api_retry / permission_denied.
  url?: string;
  repo?: string;
  identifier?: string;
  provider?: string;
  branch?: string;
  kind?: string;
  attempt?: number;
  max_retries?: number;
  error_status?: number;
  decision_reason?: string;
};

export function parseSdkSignal(text: string): SdkSignal | null {
  try {
    const parsed = JSON.parse(text) as SdkSignal;
    return parsed && typeof parsed === "object" && typeof parsed.sdk === "string" ? parsed : null;
  } catch {
    return null;
  }
}

// Session-init payload only — null for a signal entry, so callers can't
// mistake a tool_progress for "the session started with no model".
export function parseSdkSystemInfo(text: string): SdkSystemInfo | null {
  try {
    const parsed = JSON.parse(text) as SdkSystemInfo & { sdk?: unknown };
    if (!parsed || typeof parsed !== "object" || typeof parsed.sdk === "string") return null;
    return parsed;
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
  const info = latestSystemInfo(entries);
  return info?.slashCommands && info.slashCommands.length > 0 ? info.slashCommands : null;
}

// The most recent session-init payload, skipping signal entries. Stopping
// at the first SYSTEM entry of any kind (as this used to) silently emptied
// the command palette the moment any signal followed init.
export function latestSystemInfo(entries: TranscriptEntry[]): SdkSystemInfo | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].type !== TranscriptEntryType.SYSTEM) continue;
    const info = parseSdkSystemInfo(entries[i].text);
    if (info) return info;
  }
  return null;
}

// `kind: "thinking"` shares the ASSISTANT entry type with tool_use — the
// SDK carries a thinking block's prose on `thinking`, not `text`, and the
// worker tags it so this parser can tell the two apart.
export type SdkToolUse = { id?: string; tool?: string; input?: unknown; kind?: string; text?: string };
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

// Tools whose activity a side panel renders in full, so the feed showing them
// again as a generic row is a duplicate, not a summary. TodoWrite has had the
// TODOS panel since the console rewrite; Agent (the subagent spawner — note
// the SDK calls it "Agent", not "Task") now has the AGENTS panel, which also
// owns the four task_* signals the same run emits.
export const PANEL_OWNED_TOOLS = new Set(["TodoWrite", "Agent"]);

// The SDK signal subtypes the feed must not render as their own row. Every one
// of them is either owned by a panel or already said by a row next to it —
// rendering them is what produced ~1100 raw JSON blocks stuttering through a
// real session's feed.
//
// This is deliberately a denylist over SignalView's `default:` JSON dump, not
// an allowlist: a subtype the SDK adds next must stay visible (even as JSON)
// rather than vanish. Same trade-off, and the same direction, as the worker's
// own EPHEMERAL_SYSTEM_SUBTYPES.
export const PANEL_OWNED_SIGNALS = new Set([
  // The AGENTS panel renders these four, correlated into one row per run.
  "task_started",
  "task_progress",
  "task_updated",
  // task_notification is deliberately NOT here. It is the only one of the four
  // that carries a terminal outcome, and most of them belong to a backgrounded
  // Bash rather than a subagent — whose tool_result came back the instant it
  // was launched, so this notification is the ONLY thing that ever says the
  // command finished. Blanket-silencing it made a background command's
  // completion invisible. SessionFeed drops just the ones whose tool_use_id is
  // an Agent call, where the panel does own the outcome.
  // NOT rendered by the panel — silenced on its own merits. It re-lists every
  // running task on each change, and "task" here mostly means a backgrounded
  // Bash, whose own tool row is already in the feed. The lifecycle it carries
  // is the Bash call's lifecycle, which the row shows.
  "background_tasks_changed",
  // The whole slash-command/skill catalogue, descriptions included, re-dumped
  // on every change. SessionInitView already lists the skills and the composer
  // already autocompletes the commands.
  "commands_changed",
  // hook_response is the terminal sibling and renders; hook_started is the
  // "about to" half of the same event.
  "hook_started",
  // Belongs to a tool call, and now reaches it (see inFlightTool): the row
  // shows "running · 32s" instead of a detached line every 30 seconds.
  "tool_progress",
]);

export function buildToolCallPairs(entries: TranscriptEntry[]): ToolCallPair[] {
  // One pass to index results by tool_use_id, then one pass over the calls.
  // This used to scan every entry per tool call — quadratic over a
  // transcript that has no upper bound, recomputed on every render.
  const resultsById = new Map<string, { entry: TranscriptEntry; info: SdkToolResult }>();
  for (const entry of entries) {
    if (entry.type !== TranscriptEntryType.USER) continue;
    const info = parseSdkToolResult(entry.text);
    if (info?.toolUseId) resultsById.set(info.toolUseId, { entry, info });
  }

  const pairs: ToolCallPair[] = [];
  for (const entry of entries) {
    if (entry.type !== TranscriptEntryType.ASSISTANT) continue;
    const callInfo = parseSdkToolUse(entry.text) ?? {};
    // A thinking block shares the ASSISTANT type with tool_use; it has no
    // tool and renders as its own bubble, not as a call awaiting a result.
    if (callInfo.kind === "thinking") continue;
    // A tool whose own panel renders it in full — showing it again as a
    // generic raw-JSON tool call would just be a worse duplicate of the
    // same data.
    if (callInfo.tool && PANEL_OWNED_TOOLS.has(callInfo.tool)) continue;
    const found = callInfo.id ? resultsById.get(callInfo.id) : undefined;
    pairs.push({ call: entry, callInfo, result: found?.entry ?? null, resultInfo: found?.info ?? null });
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

// Every Edit/Write the agent made to one file, oldest first — the CHANGES
// panel's rows open this.
//
// The panel itself is fed by the sidecar's `git diff --numstat` telemetry
// (sidecar/internal/telemetry/loop.go), which carries a path and two line
// counts and no content at all; there is no GetDiff or ReadFile RPC anywhere
// on DashboardService. So the diff comes from where the console already has
// one: the tool inputs, which the SDK hands over with BOTH sides of the string
// and which the feed already renders through ToolInputView.
//
// ponytail: a file changed by Bash (sed, mv, a codegen script) has no tool
// input to diff, so it returns [] and the modal says so rather than showing an
// empty box. The upgrade path if that bites is a GetFileDiff RPC running
// `git diff -- <path>` in the sidecar, which is where ground truth actually is.
const FILE_EDIT_TOOLS = new Set(["Edit", "Write", "NotebookEdit"]);
export type FileEdit = { seq: bigint; tool: string; input: unknown };
export function fileEdits(entries: TranscriptEntry[], path: string): FileEdit[] {
  const out: FileEdit[] = [];
  for (const e of entries) {
    if (e.type !== TranscriptEntryType.ASSISTANT) continue;
    const info = parseSdkToolUse(e.text);
    if (!info?.tool || !FILE_EDIT_TOOLS.has(info.tool)) continue;
    const filePath = (info.input as { file_path?: unknown } | undefined)?.file_path;
    if (typeof filePath !== "string" || !samePath(filePath, path)) continue;
    out.push({ seq: e.seq, tool: info.tool, input: info.input });
  }
  return out;
}

// The two sides name the same file differently and neither is wrong: numstat
// prints repo-relative (`src/foo.ts`) because that is what git reports, and the
// SDK's file_path is absolute (`/workspace/dream-analyst/src/foo.ts`) because
// Claude Code requires absolute paths. Comparing them with === matches nothing,
// ever — which reads exactly like "this file has no captured diff" and would
// have made the whole modal look like a Bash-only edge case.
//
// Suffix on a path boundary, not a bare endsWith: `foo.ts` must not match
// `not-foo.ts`. Either side may be the longer one, so check both directions.
function samePath(a: string, b: string): boolean {
  return a === b || a.endsWith(`/${b}`) || b.endsWith(`/${a}`);
}

// One subagent the main agent spawned, assembled from the five places the SDK
// scatters it: the Agent tool_use (what was asked), its tool_result (that it
// finished), and task_started / task_progress / task_updated /
// task_notification (what it is doing, and how it ended).
export type SubagentRun = {
  toolUseId: string;
  subagentType: string;
  description: string;
  status: "running" | "completed" | "failed";
  summary?: string;
};
// No lastTool/tokens/toolUses here on purpose. Only task_progress carries
// them, and the worker drops task_progress before it is ever pushed
// (EPHEMERAL_SYSTEM_SUBTYPES, #182) — it wrote a durable Postgres row per
// subagent poll. Every task_progress row in production predates that fix and
// none has been written since. A field the pipeline cannot fill is worse than
// an absent one: it renders an empty line and reads as "this subagent is doing
// nothing".

// The four subtypes that describe a subagent, and the reason this is an
// explicit list rather than "any signal with a tool_use_id": tool_progress and
// permission_denied both carry one too, and keying off that alone minted a
// phantom "agent" row per Bash heartbeat and per denied tool. Caught by
// measuring the rendered panel, not by any unit test — the phantom rows are
// structurally identical to real ones.
const TASK_SIGNALS = new Set(["task_started", "task_progress", "task_updated", "task_notification"]);

// Statuses that mean "still working". Anything else that isn't "completed" is
// read as a failure (same direction as the signal denylist — an unrecognised
// terminal status is likelier to be a real failure than a silent success),
// but task_updated patches a task's fields on every transition, not only the
// last one, so the in-progress ones need carving out explicitly: without this
// one interim patch paints a healthy run red *permanently*, since the
// tool_result promotion below only ever upgrades a run still marked running.
const IN_PROGRESS_STATUSES = new Set(["pending", "queued", "started", "running", "in_progress"]);

// Correlation, and why it needs two keys: task_started carries BOTH
// `tool_use_id` (the Agent call) and `task_id` (the SDK's handle), but
// task_updated — the one message that reports the terminal status — carries
// only `task_id`. So the walk builds a task_id → tool_use_id index from the
// messages that have both, and resolves task_updated through it. A run whose
// task_started never arrived still shows up from its Agent call alone.
export function subagentRuns(entries: TranscriptEntry[]): SubagentRun[] {
  const runs = new Map<string, SubagentRun>();
  const byTaskId = new Map<string, string>();
  const finishedCalls = new Set<string>();

  // Index pass first, so the main walk never depends on task_started having
  // been seen before the task_updated that needs it. Same cost, one less
  // ordering assumption — and the SDK makes no promise about signal order.
  for (const entry of entries) {
    if (entry.type !== TranscriptEntryType.SYSTEM) continue;
    const sig = parseSdkSignal(entry.text);
    if (sig && TASK_SIGNALS.has(sig.sdk) && sig.task_id && sig.tool_use_id) byTaskId.set(sig.task_id, sig.tool_use_id);
  }

  for (const entry of entries) {
    if (entry.type === TranscriptEntryType.ASSISTANT) {
      const info = parseSdkToolUse(entry.text);
      if (info?.tool !== "Agent" || !info.id) continue;
      const input = (info.input ?? {}) as { subagent_type?: unknown; description?: unknown };
      runs.set(info.id, {
        toolUseId: info.id,
        subagentType: typeof input.subagent_type === "string" ? input.subagent_type : "agent",
        description: typeof input.description === "string" ? input.description : "",
        status: "running",
      });
      continue;
    }

    // The Agent call's own tool_result: the subagent has returned, whatever
    // the task_* stream did or didn't say.
    if (entry.type === TranscriptEntryType.USER) {
      const res = parseSdkToolResult(entry.text);
      if (res?.toolUseId) finishedCalls.add(res.toolUseId);
      continue;
    }

    if (entry.type !== TranscriptEntryType.SYSTEM) continue;
    const sig = parseSdkSignal(entry.text);
    if (!sig || !TASK_SIGNALS.has(sig.sdk)) continue;

    const id = sig.tool_use_id ?? (sig.task_id ? byTaskId.get(sig.task_id) : undefined);
    if (!id) continue;

    // An Agent tool_use we have actually seen, or nothing. A task_* signal is
    // NOT evidence of a subagent: a backgrounded Bash is a "task" to the SDK
    // too, and in production it is the overwhelming majority — 162 of 186
    // task_started and 157 of 181 task_notification point at a Bash call, not
    // an Agent one. Seeding a run from the signal (as this did) put a bogus
    // "agent" row with an empty description in the panel for every background
    // command, which is ~87% of the rows it would show. The cost is that a
    // subagent whose Agent call sits below the loaded page is not listed —
    // correct, and quiet, rather than confidently wrong.
    const run = runs.get(id);
    if (!run) continue;
    if (sig.subagent_type) run.subagentType = sig.subagent_type;
    if (sig.description) run.description = sig.description;
    if (sig.summary) run.summary = sig.summary;
    const status = sig.patch?.status ?? (sig.sdk === "task_notification" ? sig.status : undefined);
    if (status)
      run.status =
        status === "completed" ? "completed" : IN_PROGRESS_STATUSES.has(status) ? "running" : "failed";
    runs.set(id, run);
  }

  // Only promote to completed from the tool_result — a run the task_* stream
  // already called failed keeps that, since "it returned" and "it worked" are
  // different claims.
  for (const [id, run] of runs) if (run.status === "running" && finishedCalls.has(id)) run.status = "completed";
  return [...runs.values()];
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

export type SdkModelUsage = {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  costUSD?: number;
};
export type SdkResultSummary = {
  subtype?: string;
  numTurns?: number;
  totalCostUsd?: number;
  durationMs?: number;
  durationApiMs?: number;
  isError?: boolean;
  usage?: {
    input_tokens?: number;
    output_tokens?: number;
    cache_read_input_tokens?: number;
    cache_creation_input_tokens?: number;
  };
  modelUsage?: Record<string, SdkModelUsage>;
  permissionDenials?: { tool_name?: string }[];
  errors?: string[];
};
export function parseSdkResultSummary(text: string): SdkResultSummary | null {
  try {
    return JSON.parse(text) as SdkResultSummary;
  } catch {
    return null;
  }
}

// --- shared derivations for the console rewrite ------------------------------
// Everything below is consumed by both form factors. It lives here, not in a
// component, for the same reason the parsers above do: the desktop and mobile
// views are separately authored, and any logic they duplicate is logic they can
// silently disagree about.

// Wall-clock label for a spine/feed entry. `created_at` is on the wire, but a
// transcript predating that field carries "" — no label beats a fake one.
export function entryTime(entry: TranscriptEntry): string | null {
  if (!entry.createdAt) return null;
  const d = new Date(entry.createdAt);
  if (Number.isNaN(d.getTime())) return null;
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// The SDK's result message carries two kinds of number and gives no hint
// which is which: per-result (num_turns, duration_ms, usage) and
// session-cumulative (total_cost_usd, duration_api_ms, modelUsage costs).
// Printed side by side on one "turn ended" line they read as one receipt,
// so a 3.9-second turn that emitted 44 tokens showed up as "$1.546 · 3.9s
// (7m24s api)" — the whole session's spend, attributed to its cheapest
// turn. Subtracting the previous result gives each line its own share;
// the running totals still live in the session header.
//
// `prev` can exceed `cur` when a session resumes and the SDK's counters
// restart, hence the clamp — a negative cost is worse than a flat 0.
export function resultDelta(cur: SdkResultSummary, prev: SdkResultSummary | null): SdkResultSummary {
  if (!prev) return cur;
  const sub = (a?: number, b?: number) => Math.max(0, (a ?? 0) - (b ?? 0));
  const modelUsage: Record<string, SdkModelUsage> = {};
  for (const [name, usage] of Object.entries(cur.modelUsage ?? {})) {
    modelUsage[name] = { ...usage, costUSD: sub(usage.costUSD, prev.modelUsage?.[name]?.costUSD) };
  }
  return {
    ...cur,
    totalCostUsd: sub(cur.totalCostUsd, prev.totalCostUsd),
    durationApiMs: sub(cur.durationApiMs, prev.durationApiMs),
    modelUsage: cur.modelUsage ? modelUsage : undefined,
  };
}

export function latestResultSummary(entries: TranscriptEntry[]): SdkResultSummary | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].type !== TranscriptEntryType.RESULT) continue;
    const parsed = parseSdkResultSummary(entries[i].text);
    if (parsed) return parsed;
  }
  return null;
}

export type InFlightTool = { tool: string; summary: string; elapsedSeconds: number | null };

// The "⟳ bash · pytest tests/cache · 32s" line on a WORKING row: the newest
// tool call with no result yet. Elapsed comes from the SDK's own tool_progress
// signal when one has arrived (that's what the field is for) and falls back to
// the entry's own timestamp, so a long call still shows a duration before its
// first progress signal.
export function inFlightTool(entries: TranscriptEntry[]): InFlightTool | null {
  const pairs = buildToolCallPairs(entries);
  for (let i = pairs.length - 1; i >= 0; i--) {
    const p = pairs[i];
    if (p.result) continue;
    const tool = p.callInfo.tool;
    if (!tool) continue;

    let elapsedSeconds: number | null = null;
    for (let j = entries.length - 1; j >= 0; j--) {
      const e = entries[j];
      if (e.type !== TranscriptEntryType.SYSTEM) continue;
      const sig = parseSdkSignal(e.text);
      // parent_tool_use_id FIRST, and it is the one that actually matches:
      // the SDK's own tool_use_id on a progress signal is synthetic and
      // suffixed ("toolu_X-heartbeat-0", "bash-progress-30"), so the
      // originating call's id only ever appears on parent_tool_use_id. This
      // matched on tool_use_id alone until now, i.e. never — every in-flight
      // row silently fell through to the wall-clock fallback below.
      if (sig?.sdk === "tool_progress" && p.callInfo.id && (sig.parent_tool_use_id ?? sig.tool_use_id) === p.callInfo.id) {
        elapsedSeconds = sig.elapsed_time_seconds ?? null;
        break;
      }
    }
    if (elapsedSeconds === null && p.call.createdAt) {
      const started = new Date(p.call.createdAt).getTime();
      if (!Number.isNaN(started)) elapsedSeconds = Math.max(0, Math.round((Date.now() - started) / 1000));
    }
    return { tool, summary: summarizeToolInput(p.callInfo.input), elapsedSeconds };
  }
  return null;
}

export type ListSummary = {
  todos: TodoItem[];
  pendingPermission: PendingPermission | null;
  // How many are pending in total, of which pendingPermission is the first.
  // Rendering only the first was right; rendering it with no indication that
  // four more are queued behind it was not — after answering, a different card
  // appears in the same pixels, and from outside there is no way to tell
  // "finished" from "three to go".
  pendingPermissionCount: number;
  pendingQuestion: TranscriptEntry | null;
  inFlight: InFlightTool | null;
};

// Everything both list views need per active session, from the one transcript
// fetch App.tsx already makes for the todo bars. Rendering a blocked session's
// actual decision in the list — rather than making the human open it to find
// out what it wants — is the whole point, and it costs no extra RPC.
//
// Only the FIRST pending permission is rendered: the list card shows one
// decision, and a session blocked on several still only needs the human to
// start somewhere. The count comes along so the card can say so.
export function listSummary(entries: TranscriptEntry[]): ListSummary {
  const permissions = findPendingPermissions(entries);
  return {
    todos: latestTodos(entries) ?? [],
    pendingPermission: permissions[0] ?? null,
    pendingPermissionCount: permissions.length,
    pendingQuestion: findPendingQuestion(entries),
    inFlight: inFlightTool(entries),
  };
}

// True when the session is stalled on a human: a permission the agent's own
// tool call is blocked on, or an unanswered question. Both detail views use
// this to decide whether the decision dock exists at all.
export function hasPendingDecision(entries: TranscriptEntry[]): boolean {
  return findPendingPermissions(entries).length > 0 || findPendingQuestion(entries) !== null;
}

export type Density = "everything" | "narrative" | "decisions";
export type FeedVisibility = { narrative: boolean; tools: boolean; quiet: boolean };

// The two console mockups deliberately disagree about the third mode, so both
// mappings live here rather than one being bent to fit the other:
//   desktop "decisions" — tier-1 cards and alarms only, nothing else
//   mobile  "calls"     — tool activity, which is what a phone screen has room
//                         to scan when you're checking on a run
// Tier-1 cards (permission/plan/question) and alarms are never gated: they're
// the reason the screen exists.
export function feedVisibility(density: Density, isMobile: boolean): FeedVisibility {
  return {
    narrative: density !== "decisions",
    tools: density === "everything" || (isMobile && density === "decisions"),
    quiet: density === "everything",
  };
}
