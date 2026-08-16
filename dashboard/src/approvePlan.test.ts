import { expect, mock, test } from "bun:test";

const calls: string[] = [];

mock.module("./connectClient", () => ({
  client: {
    setPermissionMode: async () => {
      calls.push("setPermissionMode");
    },
    respondToPermission: async () => {
      calls.push("respondToPermission");
    },
  },
}));

const { approvePlan } = await import("./approvePlan");

// The order is the whole point: the agent's next turn starts the moment its
// canUseTool promise resolves, so a mode set after the answer lands too late —
// that turn still runs in "plan", where every write is refused.
test("approve + auto sets the mode before answering the plan", async () => {
  calls.length = 0;
  await approvePlan("s1", 7n, "auto");
  expect(calls).toEqual(["setPermissionMode", "respondToPermission"]);
});

test("plain approve does not touch the permission mode", async () => {
  calls.length = 0;
  await approvePlan("s1", 7n);
  expect(calls).toEqual(["respondToPermission"]);
});
