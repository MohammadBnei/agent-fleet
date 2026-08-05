import type { Task } from "./gen/agentfleet/v1/dashboard_pb";

// ponytail: the "herd" mock design shows several fields the real backend
// doesn't expose at all — no proto field, no schema column, nothing to
// wire up later short of a real backend change (unlike needsYou/branch/
// changes, which used to live here too and are now real, see App.tsx's
// needsYouIds and TaskDetail.tsx's `branch`/`changes`). This file fakes
// only what's left, deterministically per task id (so a value doesn't
// jitter every 5s poll):
//   - todos/decisions: no backend representation of a todo list or a
//     decision log exists anywhere.
//   - model/tokens/startedAt: Task has no timestamp or model/token field
//     at all (not even created_at).
//   - idleLabel/currentActivity: no "what's it doing right now" signal
//     exists server-side.

export type TaskEnrichment = {
  idleLabel: string;
  currentActivity: string;
  model: string;
  tokens: string;
  startedAt: string;
  todos: { text: string; done: boolean }[];
  decisions: { text: string; author: string }[];
};

const TODO_POOL = [
  "read the ingest path",
  "add migration",
  "wire derivation into handler",
  "chunked resumable backfill",
  "latency regression test",
  "update docs",
  "rate limit endpoint",
  "cache layer",
];

const DECISION_POOL = [
  "enum over free text",
  "±90s join window",
  "migration must be reversible",
  "flag-gate the rollout",
  "reuse shared middleware",
];

const ACTIVITY_POOL = [
  "bash · pytest -q",
  "grep · middleware",
  "read · handler.py",
  "edit · rem.py",
];

const MODELS = ["claude-opus-4-8"];

// Small, fast, deterministic — not cryptographic, just seeds picks per id.
function hashSeed(id: string): number {
  let h = 0;
  for (let i = 0; i < id.length; i++) {
    h = (h * 31 + id.charCodeAt(i)) | 0;
  }
  return Math.abs(h);
}

function pick<T>(pool: T[], seed: number, salt: number): T {
  return pool[(seed + salt) % pool.length];
}

export function enrichTask(task: Task): TaskEnrichment {
  const seed = hashSeed(task.id);
  const todoCount = 3 + (seed % 3);
  const doneCount = seed % (todoCount + 1);
  const idleMin = 1 + (seed % 40);

  return {
    idleLabel: `idle ${idleMin}m`,
    currentActivity: pick(ACTIVITY_POOL, seed, 0),
    model: pick(MODELS, seed, 0),
    tokens: `${((seed % 900) / 10 + 5).toFixed(1)}k tokens`,
    startedAt: `${9 + (seed % 8)}:${(seed % 6) * 10 || "00"}`,
    todos: Array.from({ length: todoCount }, (_, i) => ({
      text: pick(TODO_POOL, seed, i),
      done: i < doneCount,
    })),
    decisions: Array.from({ length: 1 + (seed % 3) }, (_, i) => ({
      text: pick(DECISION_POOL, seed, i),
      author: i % 2 === 0 ? "agent" : "you",
    })),
  };
}
