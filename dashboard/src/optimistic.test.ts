import { expect, test } from "bun:test";
import { optimistic } from "./optimistic";

test("apply runs before the call, and success keeps it", async () => {
  const log: string[] = [];
  const ok = await optimistic(
    () => log.push("apply"),
    async () => {
      log.push("call");
    },
    () => log.push("rollback"),
  );
  expect(ok).toBe(true);
  expect(log).toEqual(["apply", "call"]);
});

test("a failed call rolls back with the error", async () => {
  const log: string[] = [];
  const ok = await optimistic(
    () => log.push("apply"),
    async () => {
      throw new Error("at capacity");
    },
    (e) => log.push(`rollback:${e.message}`),
  );
  expect(ok).toBe(false);
  expect(log).toEqual(["apply", "rollback:at capacity"]);
});
