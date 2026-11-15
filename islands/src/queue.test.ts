import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

const src = readFileSync(
  new URL("./FleetAgent.svelte", import.meta.url),
  "utf8",
);

// A message to a cold thread waits on a pod boot. The island used to refuse a
// second send until the first returned; now sends queue in order and a
// decision answer blocks only itself.
test("sends and warm-capable answers go through the serial queue", () => {
  expect(src).toContain("serialQueue()");
  expect(src).toContain("serial(() => api.postMessage");
  expect(src).toContain("serial(() => fn(threadId))");
});

test("no page-wide send or answer lock is left", () => {
  expect(src).not.toContain("sending");
  expect(src).not.toContain("let busy ");
  expect(src).toContain("busySeqs.has(d.seq)");
});

test("a permission answer does not wait behind a warm", () => {
  expect(src).toMatch(/api\.respondToPermission[\s\S]{0,80}false,?\s*\)/);
});
