// DecisionDock uses no hooks, so it can be called as a plain function and its
// returned element tree inspected — no DOM, no renderer.
import { expect, test } from "bun:test";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "../gen/agentfleet/v1/transcript_pb";
import { DecisionDock, InterruptedBar } from "./DecisionDock";
import { PermissionCard } from "./PermissionCard";
import { PlanCard } from "./PlanCard";

let nextSeq = 0n;
function entry(type: TranscriptEntryType, text: string): TranscriptEntry {
  nextSeq += 1n;
  return {
    $typeName: "agentfleet.v1.TranscriptEntry",
    sessionId: "t",
    seq: nextSeq,
    from: "agent",
    text,
    type,
  } as TranscriptEntry;
}

const permission = (tool: string, input: unknown = {}) =>
  entry(
    TranscriptEntryType.PERMISSION_REQUEST,
    JSON.stringify({ tool, input }),
  );

function render(
  entries: TranscriptEntry[],
  extra: Partial<Parameters<typeof DecisionDock>[0]> = {},
) {
  const tree = DecisionDock({
    entries,
    busyKey: null,
    compact: true,
    onRespond: () => {},
    onAnswer: () => {},
    onPlanFeedback: () => {},
    ...extra,
  });

  const found: { type: unknown; props: Record<string, unknown> }[] = [];
  const walk = (node: unknown): void => {
    if (Array.isArray(node)) {
      node.forEach(walk);
      return;
    }
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
  const plan = permission("ExitPlanMode", {
    plan: "## Step one\n\nDo the thing.",
  });
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
  expect(
    DecisionDock({
      entries: [],
      busyKey: null,
      onRespond: () => {},
      onAnswer: () => {},
      onPlanFeedback: () => {},
    }),
  ).toBeNull();
});

const stamped = (tool: string, input: unknown = {}) =>
  entry(
    TranscriptEntryType.PERMISSION_REQUEST,
    JSON.stringify({ tool, input, nb: 1 }),
  );

// docs/adr/0082 §5: nothing blocks on a stamped request, and core warms the
// session to deliver its answer, so a dead pod no longer greys it out.
test("a stamped permission is answerable with no pod", () => {
  const cards = render([stamped("Bash", { command: "ls" })], {
    podLive: false,
  });
  const card = cards.find((el) => el.type === PermissionCard);
  expect(card?.props.pending).toBe(true);
});

// An old worker's request keeps today's rule: dead pod, dead buttons.
test("an unstamped permission on a dead pod still cannot be answered", () => {
  const cards = render([permission("Bash", { command: "ls" })], {
    podLive: false,
  });
  const card = cards.find((el) => el.type === PermissionCard);
  expect(card?.props.pending).toBe(false);
});

test("an interrupted stamped request shows interrupted, and close withdraws it", () => {
  const request = stamped("Bash", { command: "ls" });
  const withdrawn: bigint[] = [];
  const cards = render(
    [request, entry(TranscriptEntryType.INTERRUPT, "stopped")],
    { onWithdraw: (seq) => withdrawn.push(seq) },
  );
  const bar = cards.find((el) => el.type === InterruptedBar);
  expect(bar).toBeDefined();
  // Still answerable alongside the close.
  expect(cards.find((el) => el.type === PermissionCard)?.props.pending).toBe(
    true,
  );
  (bar!.props.onClose as () => void)();
  expect(withdrawn).toEqual([request.seq]);
});

test("a request with no interrupt after it shows no interrupted bar", () => {
  const cards = render([stamped("Bash")], { onWithdraw: () => {} });
  expect(cards.find((el) => el.type === InterruptedBar)).toBeUndefined();
});

// "Request changes" on a stamped plan is a deny carrying the feedback, through
// RespondToPermission: a plain reply leaves a stamped decision open (the
// worker records nothing for it), and core delivers the deny as a turn whether
// the call is still parked or long let go. An old worker's plan keeps today's
// path, a plain reply, which that worker records as the deny itself.
test("request changes on a stamped plan denies it with the feedback", () => {
  const plan = stamped("ExitPlanMode", { plan: "x" });
  const responded: unknown[] = [];
  const replied: string[] = [];
  const card = render([plan], {
    onRespond: (seq, decision) => responded.push([seq, decision]),
    onPlanFeedback: (text) => replied.push(text),
  }).find((el) => el.type === PlanCard);
  (card!.props.onFeedback as (t: string) => void)("split step two");
  expect(responded).toEqual([
    [plan.seq, { behavior: "deny", message: "split step two" }],
  ]);
  expect(replied).toEqual([]);
});

test("request changes on an unstamped plan is still a plain reply", () => {
  const responded: unknown[] = [];
  const replied: string[] = [];
  const card = render([permission("ExitPlanMode", { plan: "x" })], {
    onRespond: (seq, decision) => responded.push([seq, decision]),
    onPlanFeedback: (text) => replied.push(text),
  }).find((el) => el.type === PlanCard);
  (card!.props.onFeedback as (t: string) => void)("split step two");
  expect(replied).toEqual(["split step two"]);
  expect(responded).toEqual([]);
});
