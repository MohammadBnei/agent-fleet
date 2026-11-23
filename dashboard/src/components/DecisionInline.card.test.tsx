import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "../gen/agentfleet/v1/agent_pb";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import type { Card } from "../gen/agentfleet/v1/transcript_pb";
import { CardBody } from "./CardBody";
import { DecisionInline } from "./DecisionInline";

// A blocked session whose pending decision is a card used to render only
// "A decision is waiting in this session." in the list.

const session = { id: "s1", repo: "o/r", title: "t" } as Session;

const cardDecision = (card?: Partial<Card>): Decision =>
  ({
    id: "c1",
    kind: DecisionKind.CARD,
    state: DecisionState.OPEN,
    threadId: "s1",
    requestSeq: 3n,
    card: card && {
      $typeName: "agentfleet.v1.Card",
      ask: "Page the on-call for this alert group?",
      options: [
        { id: "1", label: "Page on-call", reversible: "yes" },
        { id: "2", label: "Silence for 1h", reversible: "yes" },
      ],
      writeOwn: false,
      draftedBy: "core",
      ...card,
    },
  }) as unknown as Decision;

const render = (card?: Decision) =>
  renderToStaticMarkup(
    <DecisionInline session={session} reload={() => {}} card={card} />,
  );

test("a card session's row shows the question and its options", () => {
  const html = render(cardDecision({}));
  expect(html).toContain("Page the on-call for this alert group?");
  expect(html).toContain("Page on-call");
  expect(html).toContain("Silence for 1h");
  expect(html).not.toContain("A decision is waiting");
});

// Same component, same default api (agent.answerDecision) as the deck and the
// session feed: answering from the row is the dock's RPC, not a second path.
test("the row answers through CardBody with the decision itself", () => {
  const d = cardDecision({});
  const tree = DecisionInline({ session, reload: () => {}, card: d });
  expect(tree.type).toBe(CardBody);
  expect(tree.props.decision).toBe(d);
  expect(tree.props.api).toBeUndefined();
});

test("a card with no payload falls back to the generic line", () => {
  expect(render(cardDecision(undefined))).toContain("A decision is waiting");
});

test("a pending permission wins over a card on the same row", () => {
  const summary = {
    pendingPermission: { toolName: "Bash", input: { command: "git push" } },
  } as unknown as Parameters<typeof DecisionInline>[0]["summary"];
  const tree = DecisionInline({
    session,
    reload: () => {},
    card: cardDecision({}),
    summary,
  });
  expect(tree.type).not.toBe(CardBody);
});

test("the row's card is keyed by its decision id", () => {
  const tree = DecisionInline({
    session,
    reload: () => {},
    card: cardDecision({}),
  });
  expect(tree.key).toBe("c1");
});
