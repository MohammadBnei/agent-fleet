import { expect, test } from "bun:test";
import {
  answerLine,
  cardChoiceLine,
  deckItems,
  listDecisionHistory,
  listOpenDecisions,
  withdrawDecision,
} from "./decisions";
import {
  type Decision,
  DecisionKind,
  DecisionState,
} from "./gen/agentfleet/v1/agent_pb";
import type { Session } from "./gen/agentfleet/v1/core_pb";

// The close button's whole contract: look the request up by its seq among the
// session's OPEN decisions, then AnswerDecision "withdraw" by id — never an
// allow or deny, and never a decision of another seq.
test("withdrawDecision withdraws the open decision of that seq, by id", async () => {
  const calls: unknown[] = [];
  const api = {
    listDecisions: async (req: unknown) => {
      calls.push(["list", req]);
      return {
        decisions: [
          { id: "d-other", requestSeq: 4n },
          { id: "d-7", requestSeq: 7n },
        ],
      };
    },
    answerDecision: async (req: unknown) => {
      calls.push(["answer", req]);
      return {};
    },
  } as unknown as Parameters<typeof withdrawDecision>[2];

  await withdrawDecision("s1", 7n, api);
  expect(calls).toEqual([
    ["list", { threadId: "s1", states: [DecisionState.OPEN], pageSize: 200 }],
    ["answer", { decisionId: "d-7", behavior: "withdraw" }],
  ]);
});

test("withdrawDecision refuses a seq that is no longer open", async () => {
  let answered = false;
  const api = {
    listDecisions: async () => ({ decisions: [] }),
    answerDecision: async () => {
      answered = true;
    },
  } as unknown as Parameters<typeof withdrawDecision>[2];

  await expect(withdrawDecision("s1", 7n, api)).rejects.toThrow(
    "no longer open",
  );
  expect(answered).toBe(false);
});

// Only the fields the deck reads; the rest default to their proto zero.
function decision(over: Partial<Decision>): Decision {
  return {
    id: "d",
    kind: DecisionKind.PERMISSION,
    state: DecisionState.OPEN,
    threadId: "s-live",
    requestSeq: 5n,
    ask: "",
    payloadJson: "{}",
    tool: "",
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

const live = {
  id: "s-live",
  repo: "o/r",
  podPhase: "POD_PHASE_RUNNING",
} as Session;
const dead = { id: "s-dead", repo: "o/r" } as Session;
const archived = {
  id: "s-arch",
  repo: "o/r",
  podPhase: "POD_PHASE_RUNNING",
  archivedAt: {},
} as Session;
const sessions = [live, dead, archived];

const question = (over: Partial<Decision> = {}) =>
  decision({
    id: "q",
    kind: DecisionKind.QUESTION,
    requestSeq: 9n,
    payloadJson: JSON.stringify({
      questions: [{ question: "which?", options: [{ label: "a" }] }],
    }),
    ...over,
  });
const permission = (stamped: boolean, over: Partial<Decision> = {}) =>
  decision({
    id: stamped ? "p-stamped" : "p",
    tool: "Bash",
    payloadJson: JSON.stringify({
      tool: "Bash",
      input: { command: "ls" },
      ...(stamped ? { nb: 1 } : {}),
    }),
    ...over,
  });

// The adapter: a Decision becomes the TranscriptEntry DecisionInline already
// takes, so listSummary does the stamp and plan detection it does today.
test("a question decision becomes the item's pending question, by its seq", () => {
  const [item] = deckItems([question()], sessions);
  expect(item.key).toBe("q");
  expect(item.session).toBe(live);
  expect(item.decision?.id).toBe("q");
  expect(item.summary?.pendingQuestion?.seq).toBe(9n);
  expect(item.summary?.pendingPermission).toBeNull();
});

test("a stamped permission carries nb and the store's interrupted flag", () => {
  const [item] = deckItems([permission(true, { interrupted: true })], sessions);
  const p = item.summary?.pendingPermission;
  expect(p?.entry.seq).toBe(5n);
  expect(p?.tool).toBe("Bash");
  expect(p?.stamped).toBe(true);
  // Set from d.interrupted, never from a faked INTERRUPT entry.
  expect(p?.interrupted).toBe(true);
});

test("a plan decision is a permission whose tool is ExitPlanMode", () => {
  const [item] = deckItems(
    [
      decision({
        id: "plan",
        kind: DecisionKind.PLAN,
        tool: "ExitPlanMode",
        payloadJson: JSON.stringify({
          tool: "ExitPlanMode",
          input: { plan: "do it" },
          nb: 1,
        }),
      }),
    ],
    sessions,
  );
  expect(item.summary?.pendingPermission?.tool).toBe("ExitPlanMode");
});

// Which open decisions the deck can answer; the same rule as transcript.ts's
// decisionAnswerable, plus the store's interrupted flag.
test("deckItems keeps only what can be answered", () => {
  const cases: [string, Decision, boolean][] = [
    ["question", question(), true],
    ["stamped permission", permission(true), true],
    ["unstamped on a live pod", permission(false), true],
    ["unstamped interrupted", permission(false, { interrupted: true }), false],
    [
      "unstamped on a dead pod",
      permission(false, { threadId: "s-dead" }),
      false,
    ],
    [
      "unstamped, unknown session",
      permission(false, { threadId: "s-gone" }),
      false,
    ],
    [
      "stamped, unknown session",
      permission(true, { threadId: "s-gone" }),
      true,
    ],
    ["question, unknown session", question({ threadId: "s-gone" }), true],
    ["question, archived session", question({ threadId: "s-arch" }), false],
  ];
  for (const [name, d, kept] of cases)
    expect([name, deckItems([d], sessions).length]).toEqual([
      name,
      kept ? 1 : 0,
    ]);
});

test("an unknown session gets a stub named by the thread", () => {
  const [item] = deckItems(
    [permission(true, { threadId: "s-gone", subjectRepo: "o/other" })],
    sessions,
  );
  expect(item.session.id).toBe("s-gone");
  expect(item.session.repo).toBe("o/other");
});

test("deckItems orders newest first", () => {
  const items = deckItems(
    [
      question({ id: "old", createdAt: "2026-10-29T09:00:00Z" }),
      question({ id: "new", createdAt: "2026-10-29T11:00:00Z" }),
      permission(true, { id: "mid", createdAt: "2026-10-29T10:00:00Z" }),
    ],
    sessions,
  );
  expect(items.map((i) => i.key)).toEqual(["new", "mid", "old"]);
});

test("answerLine says what the answer was, per kind", () => {
  const answered = (over: Partial<Decision>) =>
    decision({ state: DecisionState.ANSWERED, ...over });
  expect(
    answerLine(answered({ answerJson: JSON.stringify({ behavior: "allow" }) })),
  ).toBe("allowed");
  expect(
    answerLine(
      answered({
        answerJson: JSON.stringify({ behavior: "deny", message: "not that" }),
      }),
    ),
  ).toBe("denied: not that");
  expect(
    answerLine(
      answered({
        kind: DecisionKind.QUESTION,
        // The ANSWER row's text as the console writes it (AnswerQuestion's
        // answersJson), which core stores verbatim as answer_json.
        answerJson: JSON.stringify({ answers: { "which?": "blue" } }),
      }),
    ),
  ).toContain("blue");
  expect(answerLine(decision({ state: DecisionState.WITHDRAWN }))).toBe(
    "withdrawn",
  );
  for (const [state, line] of [
    [DecisionState.EXPIRED, "expired"],
    [DecisionState.STALE, "stale"],
    [DecisionState.OPEN, "open"],
  ] as const)
    expect(answerLine(decision({ state }))).toBe(line);
});

test("listOpenDecisions asks for one page of 200 OPEN decisions", async () => {
  const calls: unknown[] = [];
  const api = {
    listDecisions: async (req: unknown) => {
      calls.push(req);
      return { decisions: [decision({ id: "d-1" })], nextPageToken: "" };
    },
  } as unknown as Parameters<typeof listOpenDecisions>[0];
  const open = await listOpenDecisions(api);
  expect(open.map((d) => d.id)).toEqual(["d-1"]);
  expect(calls).toEqual([{ states: [DecisionState.OPEN], pageSize: 200 }]);
});

test("listDecisionHistory pages the closed states by token", async () => {
  const calls: { states: DecisionState[]; pageToken: string }[] = [];
  const api = {
    listDecisions: async (req: {
      states: DecisionState[];
      pageToken: string;
    }) => {
      calls.push(req);
      return { decisions: [decision({ id: "h-1" })], nextPageToken: "next" };
    },
  } as unknown as Parameters<typeof listDecisionHistory>[1];
  const page = await listDecisionHistory("tok", api);
  expect(page.decisions.map((d) => d.id)).toEqual(["h-1"]);
  expect(page.nextPageToken).toBe("next");
  expect(calls).toHaveLength(1);
  expect(calls[0].pageToken).toBe("tok");
  expect([...calls[0].states].sort()).toEqual(
    [
      DecisionState.ANSWERED,
      DecisionState.WITHDRAWN,
      DecisionState.EXPIRED,
      DecisionState.STALE,
    ].sort(),
  );
});

// A card as decisionToProto sends it: the Card message on Decision.card. Its
// payload_json is the same Card's protojson, which nothing here reads.
const card = (over: Partial<Decision> = {}) =>
  decision({
    id: "c1",
    kind: DecisionKind.CARD,
    requestSeq: 12n,
    ask: "PR #7 was closed by the fleet without a merge. Close its threads?",
    card: {
      $typeName: "agentfleet.v1.Card",
      ask: "PR #7 was closed by the fleet without a merge. Close its threads?",
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
          label: "Close the threads",
          effect: {
            $typeName: "agentfleet.v1.CardEffect",
            verb: "close_threads",
          },
          reversible: "no",
          recommended: false,
        },
        {
          $typeName: "agentfleet.v1.CardOption",
          id: "2",
          label: "Keep them open",
          effect: { $typeName: "agentfleet.v1.CardEffect", verb: "handoff" },
          reversible: "yes",
          recommended: false,
        },
      ],
      writeOwn: true,
      expiresAt: "2026-11-05T10:00:00Z",
      writerDefinition: "fleet",
      draftedBy: "core",
    },
    ...over,
  });

// A card never blocks a pod: core warms the writer to hand the answer over,
// so a dead pod or a session the list does not hold still shows it. Only an
// archived session keeps it out, the same rule as every other kind.
test("deckItems keeps a card on any session but an archived one", () => {
  const cases: [string, Decision, boolean][] = [
    ["card, live session", card(), true],
    ["card, dead pod", card({ threadId: "s-dead" }), true],
    ["card, unknown session", card({ threadId: "s-gone" }), true],
    ["card, archived session", card({ threadId: "s-arch" }), false],
  ];
  for (const [name, d, kept] of cases)
    expect([name, deckItems([d], sessions).length]).toEqual([
      name,
      kept ? 1 : 0,
    ]);
});

// A card is not a transcript question or request, so it gets no summary to
// answer through DecisionInline; CardBody reads the decision itself.
test("a card item carries its decision and no summary", () => {
  const [item] = deckItems([card()], sessions);
  expect(item.key).toBe("c1");
  expect(item.session).toBe(live);
  expect(item.decision?.id).toBe("c1");
  expect(item.summary).toBeUndefined();

  const [stub] = deckItems(
    [card({ threadId: "s-gone", subjectRepo: "o/other" })],
    sessions,
  );
  expect(stub.session.id).toBe("s-gone");
  expect(stub.session.repo).toBe("o/other");
  expect(stub.summary).toBeUndefined();
});

test("answerLine says which option a card's owner chose, or what they wrote", () => {
  const answered = (answerJson: unknown) =>
    card({
      state: DecisionState.ANSWERED,
      answeredBy: "human",
      answerJson: JSON.stringify(answerJson),
    });
  const ask = card().ask;
  const cases: [string, Decision, string][] = [
    [
      "handoff option",
      answered({
        card: "c1",
        optionId: "2",
        answers: {
          [ask]:
            "owner chose option 2 of card c1: Keep them open (agent claim, owner-chosen)",
        },
      }),
      "chose: Keep them open",
    ],
    [
      "close_threads option",
      answered({
        card: "c1",
        optionId: "1",
        answers: {
          [ask]:
            "owner chose option 1 of card c1: Close the threads (agent claim, owner-chosen)",
        },
      }),
      "chose: Close the threads · threads closed",
    ],
    [
      "own text",
      answered({
        card: "c1",
        ownText: true,
        answers: { [ask]: "leave PR 7 alone, I will look" },
      }),
      "wrote: leave PR 7 alone, I will look",
    ],
  ];
  for (const [name, d, line] of cases)
    expect([name, answerLine(d)]).toEqual([name, line]);
  // A closed card that was never answered says its state, as any kind does.
  expect(answerLine(card({ state: DecisionState.EXPIRED }))).toBe("expired");
  expect(answerLine(card({ state: DecisionState.STALE }))).toBe("stale");
});

// The session feed has only the card entry's protojson, not a Decision, and
// must show the same words as History, never core's message to the writer.
test("cardChoiceLine reads a card entry's options, as History does", () => {
  const options = [
    { id: "3", label: "Delete the test", effect: { verb: "handoff" } },
  ];
  const raw = (answerJson: unknown) => JSON.stringify(answerJson);
  const cases: [string, string, string][] = [
    [
      "option",
      raw({
        card: "c1",
        optionId: "3",
        answers: {
          q: "owner chose option 3 of card c1: Delete the test (agent claim, owner-chosen)",
        },
      }),
      "chose: Delete the test",
    ],
    [
      "own text",
      raw({ card: "c1", ownText: true, answers: { q: "skip it" } }),
      "wrote: skip it",
    ],
    [
      "unknown option",
      raw({ card: "c1", optionId: "9", answers: {} }),
      "answered",
    ],
    ["not json", "{", "answered"],
  ];
  for (const [name, answerJson, line] of cases)
    expect([name, cardChoiceLine(options, answerJson)]).toEqual([name, line]);
});
