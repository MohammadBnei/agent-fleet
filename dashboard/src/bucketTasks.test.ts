import { test, expect } from "bun:test";
import type { Task } from "./gen/agentfleet/v1/core_pb";
import { bucketTasks } from "./pages/TaskList";

function task(id: string, status: string): Task {
  return { $typeName: "agentfleet.v1.Task", id, status } as Task;
}

// bucketTasks splits by two hardcoded status sets, so a status in neither
// is not merely unsorted — it disappears from the task list entirely, on
// both desktop and mobile (both render from this one function). For a
// proposal that is the whole point of the feature: the human is supposed
// to see it and decide.
test("a proposed task lands in needsYou, not lost between the buckets", () => {
  const b = bucketTasks([task("a", "proposed")], new Set());

  expect(b.needsYou.map((t) => t.id)).toEqual(["a"]);
  expect(b.working).toHaveLength(0);
  expect(b.shipped).toHaveLength(0);
});

// It belongs there on its own merits, not because something flagged it —
// needsYouIds tracks outstanding transcript questions, and a proposal has
// no transcript yet.
test("a proposed task needs no needsYouIds entry", () => {
  const b = bucketTasks([task("a", "proposed")], new Set(["someone-else"]));
  expect(b.needsYou.map((t) => t.id)).toEqual(["a"]);
});

test("the existing buckets are unchanged", () => {
  const tasks = [task("run", "running"), task("ask", "running"), task("old", "done")];
  const b = bucketTasks(tasks, new Set(["ask"]));

  expect(b.needsYou.map((t) => t.id)).toEqual(["ask"]);
  expect(b.working.map((t) => t.id)).toEqual(["run"]);
  expect(b.shipped.map((t) => t.id)).toEqual(["old"]);
});
