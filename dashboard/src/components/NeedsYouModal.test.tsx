import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { DeckItem } from "../decisions";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "../gen/agentfleet/v1/agent_pb";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "../gen/agentfleet/v1/transcript_pb";
import { listSummary } from "../transcript";
import { NeedsYouModal } from "./NeedsYouModal";

// renderToStaticMarkup rather than a DOM, as Markdown.test.tsx: the question is
// only what each row says. Effects never run here, so History is handed its
// rows rather than fetched.

const live = {
  id: "s-live",
  repo: "o/r",
  title: "fix the thing",
  podPhase: "POD_PHASE_RUNNING",
} as Session;

function decision(over: Partial<Decision>): Decision {
  return {
    id: "d",
    kind: DecisionKind.PERMISSION,
    state: DecisionState.OPEN,
    threadId: live.id,
    requestSeq: 5n,
    ask: "",
    payloadJson: "{}",
    tool: "Bash",
    answerJson: "",
    answeredBy: "",
    answeredAt: "",
    createdAt: "2026-10-29T10:00:00Z",
    version: 1n,
    subjectRepo: "o/r",
    interrupted: false,
    ...over,
  } as Decision;
}

// Built the way the Contract's adapter does, without depending on deckItems:
// that has its own tests in decisions.test.ts.
function permissionItem(stamped: boolean, interrupted: boolean): DeckItem {
  const payload = JSON.stringify({
    tool: "Bash",
    input: { command: "rm -rf build" },
    ...(stamped ? { nb: 1 } : {}),
  });
  const d = decision({ id: `p-${stamped}`, payloadJson: payload, interrupted });
  const summary = listSummary([
    {
      $typeName: "agentfleet.v1.TranscriptEntry",
      sessionId: live.id,
      seq: d.requestSeq,
      from: "agent",
      text: payload,
      type: TranscriptEntryType.PERMISSION_REQUEST,
    } as TranscriptEntry,
  ]);
  if (summary.pendingPermission)
    summary.pendingPermission.interrupted = interrupted;
  return { key: d.id, session: live, summary, decision: d };
}

const noop = () => {};

function render(props: Record<string, unknown>) {
  const all = {
    open: true,
    items: [],
    available: true,
    onClose: noop,
    onOpenSession: noop,
    reload: noop,
    ...props,
  };
  // The builder may still be settling the prop shape; integration reconciles.
  return renderToStaticMarkup(
    <NeedsYouModal {...(all as Parameters<typeof NeedsYouModal>[0])} />,
  );
}

test("an open row names its kind, labels the agent's text and badges interrupted", () => {
  const html = render({ items: [permissionItem(true, true)] });
  expect(html).toContain("Permission · waiting on you");
  expect(html).toContain("Agent claim");
  expect(html).toContain("interrupted");
  // Its close is the interrupted bar's; the sheet adds no withdraw of its own.
  expect(html).not.toMatch(/>\s*withdraw\s*</i);
});

// Withdrawing a live unstamped request hangs the old worker that is blocked
// on it, so that row offers no close at all.
test("an unstamped live row has no withdraw control", () => {
  const html = render({ items: [permissionItem(false, false)] });
  expect(html).toContain("Permission · waiting on you");
  expect(html).not.toContain("Close this request without answering it");
  expect(html).not.toMatch(/>\s*withdraw\s*</i);
});

// A stamped request's worker has already moved on, so closing it hangs nothing.
test("a stamped, not interrupted permission row offers withdraw", () => {
  const html = render({ items: [permissionItem(true, false)] });
  expect(html).toContain("Close this request without answering it");
});

// The fallback (store unavailable) has no decision to withdraw by id.
test("a fallback row without a decision has no withdraw control", () => {
  const { decision: _d, ...item } = permissionItem(true, false);
  const html = render({ items: [item], available: false });
  expect(html).toContain("Permission · waiting on you");
  expect(html).not.toContain("Close this request without answering it");
});

// Same stamped summary, but the decision is a question: the kind check alone
// must hold the control back.
test("a question row has no withdraw control", () => {
  const item = permissionItem(true, false);
  const d = item.decision as Decision;
  const html = render({
    items: [{ ...item, decision: { ...d, kind: DecisionKind.QUESTION } }],
  });
  expect(html).not.toContain("Close this request without answering it");
});

test("History rows say what was answered and offer no answer", () => {
  const html = render({
    initialTab: "history",
    history: [
      decision({
        id: "h-allow",
        state: DecisionState.ANSWERED,
        answerJson: JSON.stringify({ behavior: "allow" }),
      }),
      decision({
        id: "h-deny",
        state: DecisionState.ANSWERED,
        answerJson: JSON.stringify({ behavior: "deny", message: "too broad" }),
      }),
      decision({ id: "h-gone", state: DecisionState.WITHDRAWN }),
    ],
  });
  expect(html).toContain("allowed");
  expect(html).toContain("denied: too broad");
  expect(html).toContain("withdrawn");
  expect(html).not.toMatch(/>\s*(allow|deny|approve[^<]*)\s*</);
});

test("an empty Open tab says nothing is waiting", () => {
  const html = render({ items: [] });
  expect(html).toMatch(/nothing waiting|no open decisions|no decisions/i);
  expect(html).not.toContain("cards");
});

test("with the store unavailable, History says so", () => {
  const html = render({ available: false, initialTab: "history" });
  expect(html).toMatch(/unavailable/i);
});

// A card has no summary (deckItems gives it none): CardBody answers it from
// the decision, and a card is never withdrawn from the sheet. It closes by an
// answer, by expiring, or by a newer card on the same subject.
test("a card row names its kind, shows the ask and offers no withdraw", () => {
  const ask = "Retry the flaky deploy on PR #7?";
  const d = decision({
    id: "c1",
    kind: DecisionKind.CARD,
    requestSeq: 12n,
    ask,
    tool: "",
    card: {
      $typeName: "agentfleet.v1.Card",
      ask,
      subject: {
        $typeName: "agentfleet.v1.CardSubject",
        repo: "o/r",
        kind: "pr",
        number: 7n,
        url: "https://github.com/o/r/pull/7",
      },
      options: [
        {
          $typeName: "agentfleet.v1.CardOption",
          id: "1",
          label: "Retry once",
          effect: { $typeName: "agentfleet.v1.CardEffect", verb: "handoff" },
          reversible: "yes",
          recommended: false,
        },
        {
          $typeName: "agentfleet.v1.CardOption",
          id: "2",
          label: "Leave it",
          effect: { $typeName: "agentfleet.v1.CardEffect", verb: "handoff" },
          reversible: "yes",
          recommended: false,
        },
      ],
      writeOwn: false,
      expiresAt: "2026-11-05T10:00:00Z",
      writerDefinition: "fleet",
      draftedBy: "agent",
    },
  });
  const html = render({ items: [{ key: d.id, session: live, decision: d }] });
  expect(html).toContain("Card · waiting on you");
  expect(html).toContain(ask);
  expect(html).toContain("Agent claim");
  expect(html).toContain("Retry once");
  expect(html).not.toContain("Close this request without answering it");
  expect(html).not.toMatch(/>\s*withdraw\s*</i);
});

test("History says which option a card's owner chose", () => {
  const html = render({
    initialTab: "history",
    history: [
      decision({
        id: "h-card",
        kind: DecisionKind.CARD,
        state: DecisionState.ANSWERED,
        answeredBy: "human",
        ask: "Close its threads?",
        answerJson: JSON.stringify({
          card: "h-card",
          ownText: true,
          answers: { "Close its threads?": "not yet" },
        }),
      }),
    ],
  });
  expect(html).toContain("wrote: not yet");
});
