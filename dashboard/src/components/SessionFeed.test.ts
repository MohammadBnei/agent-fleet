// SessionFeed itself uses no hooks (only its ToolGroup child does), so the
// entry walk can be called as a plain function and its returned element
// tree inspected — no DOM, no renderer. That covers the two things the walk
// alone decides: which session-init lines are worth showing, and what each
// "turn ended" line is charged.
import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { feedVisibility, type SdkResultSummary } from "../transcript";
import { SessionFeed } from "./SessionFeed";
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
