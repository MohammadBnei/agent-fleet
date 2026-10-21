import { expect, mock, test } from "bun:test";
import { Code, ConnectError } from "@connectrpc/connect";

const calls: string[] = [];
let respondError: Error | undefined;

mock.module("./connectClient", () => ({
  client: {
    setPermissionMode: async () => {
      calls.push("setPermissionMode");
    },
    respondToPermission: async () => {
      calls.push("respondToPermission");
      if (respondError) throw respondError;
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

// One reply per prompt (migration 000015): the worker records the mode
// switch's own resolution, so the answer can arrive second.
test("approve + auto treats an already-recorded answer as success", async () => {
  respondError = new ConnectError("already answered", Code.AlreadyExists);
  try {
    await approvePlan("s1", 7n, "auto");
  } finally {
    respondError = undefined;
  }
});

test("plain approve still surfaces AlreadyExists", async () => {
  respondError = new ConnectError("already answered", Code.AlreadyExists);
  try {
    await expect(approvePlan("s1", 7n)).rejects.toThrow("already answered");
  } finally {
    respondError = undefined;
  }
});
