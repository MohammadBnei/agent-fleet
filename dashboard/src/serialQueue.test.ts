import { expect, test } from "bun:test";
import { serialQueue } from "./serialQueue";

const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

test("jobs run one at a time, in the order queued", async () => {
  const enqueue = serialQueue();
  const log: string[] = [];
  const job = (name: string, ms: number) => async () => {
    log.push(`start ${name}`);
    await wait(ms);
    log.push(`end ${name}`);
    return name;
  };
  // The first is slower: without the queue the second would finish first.
  const out = await Promise.all([
    enqueue(job("a", 20)),
    enqueue(job("b", 1)),
    enqueue(job("c", 1)),
  ]);
  expect(out).toEqual(["a", "b", "c"]);
  expect(log).toEqual([
    "start a",
    "end a",
    "start b",
    "end b",
    "start c",
    "end c",
  ]);
});

test("a failed job rejects its own caller and does not block the next", async () => {
  const enqueue = serialQueue();
  const bad = enqueue(async () => {
    await wait(5);
    throw new Error("warm failed");
  });
  const good = enqueue(async () => "sent");
  await expect(bad).rejects.toThrow("warm failed");
  expect(await good).toBe("sent");
});
