import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "../gen/agentfleet/v1/agent_pb";
import type { Card, CardOption } from "../gen/agentfleet/v1/transcript_pb";
import {
  CardBody,
  notDeliveredText,
  OWN_TEXT_MAX_BYTES,
  ownBytes,
  pickArms,
} from "./CardBody";

// renderToStaticMarkup rather than a DOM, as NeedsYouModal.test.tsx: these
// check what a card shows before anyone touches it. Nothing is sent here.

const option = (
  id: string,
  label: string,
  verb: string,
  reversible: string,
  recommended = false,
): CardOption =>
  ({
    $typeName: "agentfleet.v1.CardOption",
    id,
    label,
    effect: { $typeName: "agentfleet.v1.CardEffect", verb },
    reversible,
    recommended,
  }) as CardOption;

function card(over: Partial<Card> = {}): Decision {
  const c = {
    $typeName: "agentfleet.v1.Card",
    ask: "Retry the flaky deploy on PR #7?",
    subject: {
      $typeName: "agentfleet.v1.CardSubject",
      repo: "o/r",
      kind: "pr",
      number: 7n,
      url: "https://github.com/o/r/pull/7",
    },
    options: [
      option("1", "Retry once", "handoff", "yes", true),
      option("2", "Revert the change", "handoff", "partial"),
      option("3", "Close the threads", "close_threads", "no"),
    ],
    writeOwn: false,
    expiresAt: "2026-11-05T10:00:00Z",
    writerDefinition: "fleet",
    draftedBy: "agent",
    ...over,
  } as Card;
  return {
    id: "c1",
    kind: DecisionKind.CARD,
    state: DecisionState.OPEN,
    threadId: "s-live",
    requestSeq: 12n,
    ask: c.ask,
    payloadJson: "{}",
    tool: "",
    answerJson: "",
    answeredBy: "",
    answeredAt: "",
    createdAt: "2026-10-29T10:00:00Z",
    version: 1n,
    subjectRepo: "o/r",
    interrupted: false,
    card: c,
  } as Decision;
}

function render(d: Decision, extra: Record<string, unknown> = {}) {
  // The builder may still be settling the prop shape; integration reconciles.
  const props = {
    decision: d,
    reload: () => {},
    onNotDelivered: () => {},
    ...extra,
  };
  return renderToStaticMarkup(
    <CardBody {...(props as Parameters<typeof CardBody>[0])} />,
  );
}

test("a card shows its ask as an agent claim, its subject link and its expiry", () => {
  const html = render(card());
  expect(html).toContain("Retry the flaky deploy on PR #7?");
  expect(html).toContain("Agent claim");
  expect(html).toContain('href="https://github.com/o/r/pull/7"');
  expect(html).toMatch(/expires/i);
});

// Core's own cards (dispatch's close-threads card) are not an agent's words.
test("a card core drafted says so instead of agent claim", () => {
  const html = render(card({ draftedBy: "core" }));
  expect(html).toContain("From core");
  expect(html).not.toContain("Agent claim");
});

// Never pre-select: not the recommended option, not the first one. Every
// option is a plain button the owner has to press.
test("nothing is preselected, the recommended option only wears a badge", () => {
  const html = render(card());
  expect(html).not.toMatch(
    /aria-(pressed|checked|selected)="true"|\schecked=""|\sselected=""|autofocus/i,
  );
  for (const label of ["Retry once", "Revert the change", "Close the threads"])
    expect(html).toMatch(
      new RegExp(`<button[^>]*>[^]*?${label}[^]*?</button>`),
    );
  expect(html).toMatch(/recommended/i);
  expect(html.match(/recommended/gi)?.length).toBe(1);
});

// The agent writes the label; core writes what the option actually does, so
// a label cannot hide the effect.
test("each option shows its reversibility and core's own effect words", () => {
  const html = render(card());
  expect(html).toContain("partly reversible");
  expect(html).toContain("irreversible");
  expect(html).toMatch(/(^|[^y])reversible/);
  expect(html).toContain("Sends your choice to the agent");
  expect(html).toContain("Closes every thread on");
});

// The first click on an irreversible option only arms it; nothing is sent
// until the confirm.
test("an irreversible option is not armed until it is clicked", () => {
  const html = render(card());
  expect(html).not.toContain("This cannot be undone");
  expect(html).not.toContain("Confirm: Close the threads");
});

// The click itself: an irreversible option arms on the first click and sends
// on the second click on it; another option armed does not count.
test("an irreversible option sends only on its own second click", () => {
  const [, , close] = card().card!.options;
  const retry = card().card!.options[0];
  const cases: [string, CardOption, string | null, boolean][] = [
    ["first click on irreversible", close, null, true],
    ["second click on irreversible", close, "3", false],
    ["another option armed", close, "1", true],
    ["reversible", retry, null, false],
  ];
  for (const [name, o, armed, arms] of cases)
    expect([name, pickArms(o, armed)]).toEqual([name, arms]);
});

test("write your own appears only where the card allows it", () => {
  expect(render(card())).not.toContain("<textarea");
  const html = render(card({ writeOwn: true }));
  expect(html).toContain("<textarea");
  expect(html).toMatch(/>\s*Send\s*</);
});

// agentsvc owns the reason's words; the notice names the card by its subject
// and says the reason once (it once read "... cannot resume (the writer
// cannot resume)").
test("the not-delivered notice names the card's subject and says the reason once", () => {
  const c = card().card!;
  const cases: [
    string,
    Parameters<typeof notDeliveredText>[0],
    string,
    string,
  ][] = [
    [
      "reason from core",
      c,
      "the writer cannot resume",
      "o/r pr #7: not delivered: the writer cannot resume",
    ],
    ["no reason", c, "", "o/r pr #7: not delivered: the writer cannot resume"],
    [
      "no subject, long ask",
      { ask: "x".repeat(280) },
      "swept",
      `${"x".repeat(60)}…: not delivered: swept`,
    ],
  ];
  for (const [name, cd, reason, text] of cases) {
    const got = notDeliveredText(cd, reason);
    expect([name, got]).toEqual([name, text]);
    expect(got.split(reason || "cannot resume").length).toBe(2);
  }
});

// The own-text bound is the server's 2,000 bytes, not characters.
test("own text is measured in bytes", () => {
  expect(ownBytes("a".repeat(2000))).toBe(2000);
  expect(ownBytes("é".repeat(1001)) > OWN_TEXT_MAX_BYTES).toBe(true);
});
