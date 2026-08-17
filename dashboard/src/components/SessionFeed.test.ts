// SessionFeed itself uses no hooks (only its ToolGroup child does), so the
// entry walk can be called as a plain function and its returned element
// tree inspected — no DOM, no renderer. That covers the two things the walk
// alone decides: which session-init lines are worth showing, and what each
// "turn ended" line is charged.
import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { feedVisibility, type SdkResultSummary } from "../transcript";
import { LoadOlder, SessionFeed } from "./SessionFeed";
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
