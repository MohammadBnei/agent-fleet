import { test, expect } from "bun:test";
import { SessionQueue } from "./queue.js";

test("runs jobs strictly in order, never overlapping", async () => {
  const q = new SessionQueue();
  const events: string[] = [];

  const job = (name: string, ms: number) =>
    q.enqueue(async () => {
      events.push(`${name}:start`);
      await Bun.sleep(ms);
      events.push(`${name}:end`);
    });

  // b is deliberately faster than a — without serialization it would
  // finish first and interleave.
  await Promise.all([job("a", 30), job("b", 1), job("c", 1)]);

  expect(events).toEqual([
    "a:start", "a:end",
    "b:start", "b:end",
    "c:start", "c:end",
  ]);
});

test("a rejected job does not poison the ones queued behind it", async () => {
  const q = new SessionQueue();

  const failed = q.enqueue(async () => {
    throw new Error("boom");
  });
  const after = q.enqueue(async () => "survived");

  expect(failed).rejects.toThrow("boom");
  expect(await after).toBe("survived");
});

test("depth drains back to zero once everything settles", async () => {
  const q = new SessionQueue();
  const a = q.enqueue(async () => Bun.sleep(5));
  const b = q.enqueue(async () => {
    throw new Error("boom");
  });
  expect(q.depth).toBe(2);

  await a;
  await b.catch(() => undefined);
  // The decrement is scheduled on a microtask after settle, so yield once.
  await Bun.sleep(1);
  expect(q.depth).toBe(0);
});
