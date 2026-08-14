// DecisionDock uses no hooks, so it can be called as a plain function and its
// returned element tree inspected — no DOM, no renderer.
import { test, expect } from "bun:test";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { DecisionDock } from "./DecisionDock";
import { PlanCard } from "./PlanCard";
import { PermissionCard } from "./PermissionCard";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, text: string): TranscriptEntry {
  nextSeq += 1n;
  return { $typeName: "agentfleet.v1.TranscriptEntry", sessionId: "t", seq: nextSeq, from: "agent", text, type } as TranscriptEntry;
}

const permission = (tool: string, input: unknown = {}) =>
  entry(TranscriptEntryType.PERMISSION_REQUEST, JSON.stringify({ tool, input }));

function render(entries: TranscriptEntry[]) {
  const tree = DecisionDock({
    entries,
    busyKey: null,
    compact: true,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  });

  const found: { type: unknown; props: Record<string, unknown> }[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(walk);
    if (!node || typeof node !== "object") return;
    const el = node as { type?: unknown; props?: Record<string, unknown> };
    if (el.type) found.push({ type: el.type, props: el.props ?? {} });
    if (el.props?.children) walk(el.props.children);
  };
  walk(tree);
  return found;
}

// The dock caps its own height and scrolls; a plan body that also scrolls
// makes two nested scrollers, and on a phone the inner one was taller than
// the dock — so dragging the plan text never moved the dock, and approve /
// request changes stayed permanently below the fold with no way to reach
// them. `docked` is what tells PlanCard to leave the scrolling to the dock.
test("a docked plan leaves the scrolling to the dock", () => {
  const plan = permission("ExitPlanMode", { plan: "## Step one\n\nDo the thing." });
  const card = render([plan]).find((el) => el.type === PlanCard);

  expect(card).toBeDefined();
  expect(card!.props.docked).toBe(true);
  expect(card!.props.pending).toBe(true);
  expect(card!.props.plan).toContain("Do the thing.");
});

// Everything else in the dock is short enough to live inside its scroll
// bound, so only the plan needs this.
test("a non-plan permission still renders its own card", () => {
  const cards = render([permission("Edit", { file_path: "a.py" })]);
  expect(cards.find((el) => el.type === PermissionCard)).toBeDefined();
  expect(cards.find((el) => el.type === PlanCard)).toBeUndefined();
});

test("an empty dock renders nothing at all", () => {
  expect(DecisionDock({
    entries: [],
    busyKey: null,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
  })).toBeNull();
});
