import { expect, test } from "bun:test";
import { withdrawDecision } from "./decisions";
import { DecisionState } from "./gen/agentfleet/v1/agent_pb";

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
