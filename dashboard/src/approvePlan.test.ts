import { expect, test } from "bun:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { approvePlan, planFeedback } from "./approvePlan";

const calls: string[] = [];
let respondError: Error | undefined;

// Typed loosely on purpose: approvePlan only reads these two methods.
const api = {
  setPermissionMode: async () => {
    calls.push("setPermissionMode");
  },
  respondToPermission: async () => {
    calls.push("respondToPermission");
    if (respondError) throw respondError;
  },
} as unknown as Parameters<typeof approvePlan>[3];

// The order is the whole point: the agent's next turn starts the moment its
// canUseTool promise resolves, so a mode set after the answer lands too late —
// that turn still runs in "plan", where every write is refused.
test("approve + auto sets the mode before answering the plan", async () => {
  calls.length = 0;
  await approvePlan("s1", 7n, "auto", api);
  expect(calls).toEqual(["setPermissionMode", "respondToPermission"]);
});

test("plain approve does not touch the permission mode", async () => {
  calls.length = 0;
  await approvePlan("s1", 7n, undefined, api);
  expect(calls).toEqual(["respondToPermission"]);
});

// One reply per prompt (migration 000015): the worker records the mode
// switch's own resolution, so the answer can arrive second.
test("approve + auto treats an already-recorded answer as success", async () => {
  respondError = new ConnectError("already answered", Code.AlreadyExists);
  try {
    await approvePlan("s1", 7n, "auto", api);
  } finally {
    respondError = undefined;
  }
});

test("plain approve still surfaces AlreadyExists", async () => {
  respondError = new ConnectError("already answered", Code.AlreadyExists);
  try {
    await expect(approvePlan("s1", 7n, undefined, api)).rejects.toThrow(
      "already answered",
    );
  } finally {
    respondError = undefined;
  }
});

// "Request changes" on a plan. A stamped plan has no worker blocked on it, so a
// plain message would leave the plan open forever: it is a deny carrying the
// feedback, as DecisionDock does. A live unstamped plan takes a message, which
// the worker reads as deny-with-feedback.
function feedbackApi() {
  const seen: [string, unknown][] = [];
  const api = {
    respondToPermission: async (req: unknown) => {
      seen.push(["respond", req]);
    },
    postMessage: async (req: unknown) => {
      seen.push(["post", req]);
    },
  } as unknown as Parameters<typeof planFeedback>[4];
  return { seen, api };
}

test("planFeedback on a stamped plan denies with the feedback", async () => {
  const { seen, api } = feedbackApi();
  await planFeedback("s1", 7n, true, "smaller steps", api);
  expect(seen).toHaveLength(1);
  const [kind, req] = seen[0] as [
    string,
    { sessionId: string; seq: bigint; decisionJson: string },
  ];
  expect(kind).toBe("respond");
  expect(req.sessionId).toBe("s1");
  expect(req.seq).toBe(7n);
  expect(JSON.parse(req.decisionJson)).toEqual({
    behavior: "deny",
    message: "smaller steps",
  });
});

test("planFeedback on an unstamped plan only posts a message", async () => {
  const { seen, api } = feedbackApi();
  await planFeedback("s1", 7n, false, "smaller steps", api);
  expect(seen).toEqual([["post", { sessionId: "s1", text: "smaller steps" }]]);
});
