import { expect, test } from "bun:test";
import { layout } from "./FleetTopology";
import type { CellNode } from "../gen/agentfleet/v1/dashboard_pb";

function node(id: string, type: string): CellNode {
  return {
    $typeName: "agentfleet.v1.CellNode",
    id,
    type,
    status: "healthy",
    metrics: {},
    taskId: "",
    repo: "",
    label: "",
  };
}

// An empty fleet is the normal state, and it must not divide by zero into
// NaN coordinates — an SVG with NaN in a transform renders nothing at all,
// silently.
test("hubs are placed even with no worker cells", () => {
  const placed = layout([node("core", "core"), node("provisioner", "provisioner")]);
  expect(placed).toHaveLength(2);
  for (const p of placed) {
    expect(Number.isFinite(p.x)).toBe(true);
    expect(Number.isFinite(p.y)).toBe(true);
  }
  // Both hubs share the centre column, on different tiers.
  expect(placed[0].x).toBe(placed[1].x);
  expect(placed[0].y).not.toBe(placed[1].y);
});

// A single worker centres rather than pinning to the left edge, and N
// workers spread without overlapping.
test("workers spread evenly across the row", () => {
  const one = layout([node("core", "core"), node("worker/a", "worker")]);
  const solo = one.find((p) => p.type === "worker")!;
  expect(solo.x + solo.w / 2).toBeCloseTo(one[0].x + one[0].w / 2, 5);

  const many = layout([
    node("core", "core"),
    node("provisioner", "provisioner"),
    ...["a", "b", "c"].map((k) => node(`worker/${k}`, "worker")),
  ]);
  const workers = many.filter((p) => p.type === "worker");
  expect(workers).toHaveLength(3);
  const xs = workers.map((w) => w.x);
  expect(xs).toEqual([...xs].sort((a, b) => a - b));
  for (let i = 1; i < xs.length; i++) {
    expect(xs[i] - xs[i - 1]).toBeGreaterThanOrEqual(workers[0].w);
  }
  // Every worker shares one tier.
  expect(new Set(workers.map((w) => w.y)).size).toBe(1);
});
