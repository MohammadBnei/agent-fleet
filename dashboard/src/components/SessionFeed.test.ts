// SessionFeed itself uses no hooks (only its ToolGroup child does), so the
// entry walk can be called as a plain function and its returned element
// tree inspected — no DOM, no renderer. That covers the two things the walk
// alone decides: which session-init lines are worth showing, and what each
// "turn ended" line is charged.
import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { feedVisibility, type SdkResultSummary } from "../transcript";
import { LoadOlder, SessionFeed, shortTool } from "./SessionFeed";
import { TranscriptEntryView } from "./TranscriptEntryView";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, text: string): TranscriptEntry {
  nextSeq += 1n;
  return { $typeName: "agentfleet.v1.TranscriptEntry", sessionId: "t", seq: nextSeq, from: "agent", text, type } as TranscriptEntry;
}

const init = (model: string, tools = 33) =>
  entry(
    TranscriptEntryType.SYSTEM,
    JSON.stringify({
      model,
      permissionMode: "default",
      tools: Array.from({ length: tools }, (_, i) => `tool-${i}`),
      mcpServers: [{ name: "agent-fleet-sidecar", status: "connected" }],
    }),
  );

const result = (totalCostUsd: number, durationApiMs: number, durationMs: number) =>
  entry(TranscriptEntryType.RESULT, JSON.stringify({ subtype: "success", numTurns: 1, totalCostUsd, durationApiMs, durationMs }));

type Rendered = { entry: TranscriptEntry; result?: SdkResultSummary | null };

function renderedEntryViews(entries: TranscriptEntry[]): Rendered[] {
  const tree = SessionFeed({
    entries,
    visibility: feedVisibility("everything", false),
    density: "everything",
    busyKey: null,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });

  const found: Rendered[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (!node || typeof node !== "object") return;
    const el = node as { type?: unknown; props?: Record<string, unknown> };
    if (el.type === TranscriptEntryView) found.push(el.props as Rendered);
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);
  return found;
}

// The cross-session branch renders raw JSX rather than a TranscriptEntryView,
// so renderedEntryViews above cannot see it — flatten the tree to text instead.
function renderedText(entries: TranscriptEntry[]): string {
  const tree = SessionFeed({
    entries,
    visibility: feedVisibility("everything", false),
    density: "everything",
    busyKey: null,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });

  let text = "";
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (typeof node === "string" || typeof node === "number") {
      text += String(node);
      return;
    }
    if (!node || typeof node !== "object") return;
    const el = node as { props?: Record<string, unknown> };
    // TranscriptEntryView renders through a real component, so its own text
    // never reaches this walk — that is what keeps the assertions below
    // specific to the cross-session branch.
    if (el.props?.text) text += String(el.props.text);
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);
  return text;
}

const peerEntry = (text: string) =>
  ({ ...entry(TranscriptEntryType.DISCUSSION, text), from: "session" }) as TranscriptEntry;

test("a peer session's message is attributed to its sender", () => {
  expect(renderedText([peerEntry("[from session abcdef123456]\n\nwhich schema?")])).toContain("#abcdef");
});

test("a peer message with no prefix is still rendered as a peer message", () => {
  const text = renderedText([peerEntry("which schema?")]);
  expect(text).toContain("another session");
  // Not "from #" — an empty id sliced to 6 chars is what the old truthiness
  // check would have produced if it had reached this branch at all.
  expect(text).not.toContain("#");
});

// The regex used to be tested against every entry's text and never against its
// author, so a human writing that literal was rendered as another agent — the
// one attribution in the feed a human cannot forge is exactly the one that was
// forgeable.
test("a human message that starts with the cross-session literal is not attributed to a session", () => {
  const human = { ...entry(TranscriptEntryType.DISCUSSION, "[from session deadbeef]\n\nnot me"), from: "human" } as TranscriptEntry;
  expect(renderedText([human])).not.toContain("#deadbe");
});

// The SDK re-emits system/init at the head of every turn in streaming-input
// mode. Labelling each one "session started" put a false session boundary
// between every prompt and its answer — three of them in one unbroken
// session, in the exchange that prompted this.
test("an unchanged session-init is not re-announced, a changed one is", () => {
  const entries = [init("opus"), result(1.0, 100, 10), init("opus"), result(1.1, 200, 20), init("sonnet"), result(0.2, 50, 5)];

  const inits = renderedEntryViews(entries).filter((r) => r.entry.type === TranscriptEntryType.SYSTEM);
  expect(inits.length).toBe(2);
  expect(JSON.parse(inits[0].entry.text).model).toBe("opus");
  expect(JSON.parse(inits[1].entry.text).model).toBe("sonnet");
});

test("each turn's line is charged its own share, and a changed init resets the baseline", () => {
  const entries = [init("opus"), result(1.0, 100_000, 10_000), result(1.1, 180_000, 20_000), init("sonnet"), result(0.2, 5_000, 5_000)];

  const results = renderedEntryViews(entries).filter((r) => r.entry.type === TranscriptEntryType.RESULT);
  expect(results.length).toBe(3);

  // First of the session: nothing to subtract.
  expect(results[0].result?.totalCostUsd).toBeCloseTo(1.0, 5);
  expect(results[0].result?.durationApiMs).toBe(100_000);

  // Second: its own share, not the running total.
  expect(results[1].result?.totalCostUsd).toBeCloseTo(0.1, 5);
  expect(results[1].result?.durationApiMs).toBe(80_000);
  expect(results[1].result?.durationMs).toBe(20_000); // per-result, untouched

  // Third follows a real re-init, so the SDK's counters restarted with it.
  expect(results[2].result?.totalCostUsd).toBeCloseTo(0.2, 5);
  expect(results[2].result?.durationApiMs).toBe(5_000);
});

// The feed opens on the newest page, so the way back through history is the
// header at its head. It renders only when the hook says there is more behind
// what's held — a session whose whole transcript fits in one page must not
// offer a fetch that returns nothing.
test("the load-earlier header appears only when older history exists", () => {
  const props = {
    entries: [init("opus")],
    visibility: feedVisibility("everything", false),
    density: "everything" as const,
    busyKey: null,
    onRespond: () => {},
    onApprovePlan: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  };

  const hasLoadOlder = (tree: unknown): boolean => {
    let found = false;
    const walk = (node: unknown): void => {
      if (Array.isArray(node)) return node.forEach(walk);
      if (!node || typeof node !== "object") return;
      const el = node as { type?: unknown; props?: Record<string, unknown> };
      if (el.type === LoadOlder) found = true;
      if (el.props?.children) walk(el.props.children);
    };
    walk(tree);
    return found;
  };

  expect(hasLoadOlder(SessionFeed({ ...props, hasOlder: true, onLoadOlder: () => {} }))).toBe(true);
  // No older history…
  expect(hasLoadOlder(SessionFeed({ ...props, hasOlder: false, onLoadOlder: () => {} }))).toBe(false);
  // …and no handler to fetch it with (a surface that doesn't paginate).
  expect(hasLoadOlder(SessionFeed({ ...props, hasOlder: true }))).toBe(false);
});

// --- the signal denylist ------------------------------------------------------
// These twelve subtypes accounted for ~1100 raw JSON blocks across 15 real
// sessions, because SignalView's default branch dumps anything it has no case
// for. This list is the thing that rots as the SDK adds subtypes, so it gets a
// test rather than a comment.

const sig = (payload: Record<string, unknown>) => entry(TranscriptEntryType.SYSTEM, JSON.stringify(payload));

test("panel-owned and duplicate signals render nothing in the feed", () => {
  for (const payload of [
    { sdk: "task_started", task_id: "t", tool_use_id: "toolu_1", prompt: "a very long subagent prompt" },
    { sdk: "task_progress", task_id: "t", tool_use_id: "toolu_1", last_tool_name: "Read" },
    { sdk: "task_updated", task_id: "t", patch: { status: "completed" } },
    { sdk: "background_tasks_changed", tasks: [{ task_id: "t" }] },
    { sdk: "commands_changed", commands: [{ name: "ponytail", description: "…" }] },
    { sdk: "hook_started", hook_name: "SessionStart:startup" },
    { sdk: "tool_progress", tool_use_id: "toolu_1-heartbeat-0", parent_tool_use_id: "toolu_1", elapsed_time_seconds: 30 },
  ]) {
    expect(renderedEntryViews([sig(payload)])).toHaveLength(0);
  }
});

// api_retry carries error:"overloaded", which used to make it an alarm bar —
// ten of those per stall out-shout the auth failure tier 5 exists for. It must
// still render, though: it is the answer to "why has this gone quiet".
test("api_retry renders as a log line, not an alarm bar", () => {
  const views = renderedEntryViews([sig({ sdk: "api_retry", attempt: 1, max_retries: 10, error_status: 529, error: "overloaded" })]);
  expect(views).toHaveLength(1);
  expect(renderedText([sig({ sdk: "api_retry", attempt: 1, error: "overloaded" })])).not.toContain("!");
});

test("an unknown subtype still reaches the feed rather than being swallowed", () => {
  expect(renderedEntryViews([sig({ sdk: "some_subtype_the_sdk_added_next", detail: 1 })])).toHaveLength(1);
});

// A fixed-width flex item does not clip without overflow-hidden, so
// `mcp__playwright__browser_navigate` drew straight over the summary column
// beside it. `truncate` supplies it; shortTool makes the visible text fit.
test("shortTool drops the MCP server prefix and leaves plain tools alone", () => {
  expect(shortTool("mcp__agent-fleet-sidecar__set_session_meta")).toBe("set_session_meta");
  expect(shortTool("Bash")).toBe("Bash");
  expect(shortTool(undefined)).toBe("tool");
});

// The elapsed seconds that used to arrive as a standalone tool_progress row
// have to land ON the tool row now that the row is the only thing rendered.
// Keyed by parent_tool_use_id — the signal's own tool_use_id is suffixed per
// emission and matches no call, which is how the identical lookup in
// inFlightTool stayed dead from the day it was written.
test("a silenced tool_progress still reaches its tool row as elapsed time", () => {
  const tree = SessionFeed({
    entries: [
      entry(TranscriptEntryType.ASSISTANT, JSON.stringify({ id: "toolu_E", tool: "Bash", input: { command: "go test ./..." } })),
      sig({ sdk: "tool_progress", tool_use_id: "toolu_E-heartbeat-0", parent_tool_use_id: "toolu_E", tool_name: "Bash", elapsed_time_seconds: 90 }),
    ],
    visibility: feedVisibility("everything", false),
    density: "everything",
    busyKey: null,
    onRespond: () => {},
    onApprovePlan: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });

  const groups: { elapsedById: Map<string, number> }[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (!node || typeof node !== "object") return;
    const el = node as { type?: unknown; props?: Record<string, unknown> };
    if (el.props?.elapsedById) groups.push(el.props as { elapsedById: Map<string, number> });
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);

  expect(groups).toHaveLength(1);
  expect(groups[0].elapsedById.get("toolu_E")).toBe(90);
});

// task_notification is the only one of the four that carries a terminal
// outcome, and most belong to a backgrounded Bash — whose tool_result returned
// the instant it launched, so this is the ONLY thing that ever says the
// command finished. Silencing it wholesale made that invisible.
test("a subagent's task_notification is silenced but a background command's is not", () => {
  const agentCall = entry(TranscriptEntryType.ASSISTANT, JSON.stringify({ id: "toolu_A", tool: "Agent", input: { subagent_type: "Explore" } }));
  const forAgent = sig({ sdk: "task_notification", task_id: "t", tool_use_id: "toolu_A", status: "completed", summary: "agent done" });
  expect(renderedEntryViews([agentCall, forAgent])).toHaveLength(0);

  const bashCall = entry(TranscriptEntryType.ASSISTANT, JSON.stringify({ id: "toolu_B", tool: "Bash", input: { command: "sleep 45", run_in_background: true } }));
  const forBash = sig({ sdk: "task_notification", task_id: "t", tool_use_id: "toolu_B", status: "completed", summary: "Background command completed (exit code 0)" });
  expect(renderedEntryViews([bashCall, forBash])).toHaveLength(1);
});

// A tool_result whose tool_use is not in the loaded page. Reported from a live
// session: it rendered its JSON envelope as agent prose, so a failed build's
// stderr read like something the agent said.
//
// The cause was structural, not cosmetic — the two USER early-continues
// (already-paired, panel-owned) were the only guards, so anything neither
// caught fell all the way to the narrative tier. One guard now owns every
// tool_result and none can fall past it.
const orphanResult = () =>
  entry(
    TranscriptEntryType.USER,
    JSON.stringify({
      toolUseId: "toolu_call_is_off_this_page",
      isError: true,
      content: "Exit code 1\nmain.go:40:26: undefined: agentfleetv1.WorkerPod",
    }),
  );

// Finds the props of whatever element carries a parsed tool_result. The row
// renders through Collapse, and renderedText cannot see through a real
// component (same limitation it already has for TranscriptEntryView), so the
// rendered STRINGS are asserted in the Playwright spec instead — this pins the
// routing decision, which is where the bug was.
function orphanProps(entries: TranscriptEntry[]): { result?: { toolUseId?: string; isError?: boolean } }[] {
  const tree = SessionFeed({
    entries,
    visibility: feedVisibility("everything", false),
    density: "everything",
    busyKey: null,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });
  const found: { result?: { toolUseId?: string; isError?: boolean } }[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (!node || typeof node !== "object") return;
    const el = node as { props?: Record<string, unknown> };
    if (el.props && "result" in el.props) found.push(el.props as never);
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);
  return found;
}

test("an orphaned tool_result never renders as prose", () => {
  // The envelope — the actual reported symptom. Nothing of it reaches the
  // narrative tier, which is the only tier renderedText can see.
  const text = renderedText([orphanResult()]);
  expect(text).not.toContain("toolUseId");
  expect(text).not.toContain("toolu_call_is_off_this_page");

  // Not silently dropped either: the output is usually why someone scrolled
  // back this far. It is routed to the tool-output renderer instead.
  const props = orphanProps([orphanResult()]);
  expect(props).toHaveLength(1);
  expect(props[0].result?.toolUseId).toBe("toolu_call_is_off_this_page");
  expect(props[0].result?.isError).toBe(true);
});

// It is tool activity, so it obeys the tool density gate — appearing in a view
// that hides every other tool row would make it look like narrative again.
test("an orphaned tool_result is hidden when tool rows are", () => {
  const tree = SessionFeed({
    entries: [orphanResult()],
    visibility: { ...feedVisibility("everything", false), tools: false },
    density: "decisions",
    busyKey: null,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });
  let text = "";
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (typeof node === "string") text += node;
    if (!node || typeof node !== "object") return;
    const el = node as { props?: Record<string, unknown> };
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);
  expect(text).not.toContain("output of an earlier tool call");
  expect(orphanProps([])).toHaveLength(0);
});

// A paired result must still be folded into its tool row, not doubled by the
// new branch.
test("a paired tool_result is not also rendered as an orphan", () => {
  expect(
    orphanProps([
      entry(TranscriptEntryType.ASSISTANT, JSON.stringify({ id: "toolu_paired", tool: "Bash", input: { command: "go build ./..." } })),
      entry(TranscriptEntryType.USER, JSON.stringify({ toolUseId: "toolu_paired", isError: false, content: "ok" })),
    ]),
  ).toHaveLength(0);
});
