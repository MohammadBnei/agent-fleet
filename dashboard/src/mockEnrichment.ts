import type { Task } from "./gen/agentfleet/v1/dashboard_pb";

// ponytail: the "herd" mock design shows several fields the real backend
// doesn't expose yet — todos, decisions log, model/tokens/worktree/branch/
// session-time, and a "needs your input" signal. This file fakes all of
// them, deterministically per task id (so a value doesn't jitter every 5s
// poll), so the reskin can ship now and get wired to real data later:
//   - needsYou: should come from a backend flag once listTasks can cheaply
//     compute "latest transcript entry is an unanswered question" — see
//     the plan's deferred backend note.
//   - changes/branch: the sidecar already pushes real {branch, files[]}
//     diff snapshots via a TOOL_CALL transcript entry, but two backend
//     bugs currently drop it (db/schema.sql's type CHECK constraint
//     doesn't allow 'tool_call', and dashboard/server.go's
//     stringToProtoType has no case for it). Fix those, then swap this
//     panel to read real TOOL_CALL entries instead.
//   - todos/decisions/model/tokens/worktree/startedAt: no backend source
//     exists at all yet.

export type TaskEnrichment = {
  needsYou: boolean;
  idleLabel: string;
  currentActivity: string;
  worktree: string;
  branch: string;
  model: string;
  tokens: string;
  startedAt: string;
  todos: { text: string; done: boolean }[];
  changes: { path: string; added: number; removed: number }[];
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

const FILE_POOL = [
  "migrations/014_rem_phase.sql",
  "ingest/handler.py",
  "ingest/rem.py",
  "src/middleware/auth.ts",
  "tests/cache_test.py",
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
  const changeCount = 1 + (seed % 3);
  const idleMin = 1 + (seed % 40);

  return {
    needsYou: seed % 5 === 0,
    idleLabel: `idle ${idleMin}m`,
    currentActivity: pick(ACTIVITY_POOL, seed, 0),
    worktree: `task-${task.id.slice(0, 8)}`,
    branch: `agent/${task.id.slice(0, 8)}`,
    model: pick(MODELS, seed, 0),
    tokens: `${((seed % 900) / 10 + 5).toFixed(1)}k tokens`,
    startedAt: `${9 + (seed % 8)}:${(seed % 6) * 10 || "00"}`,
    todos: Array.from({ length: todoCount }, (_, i) => ({
      text: pick(TODO_POOL, seed, i),
      done: i < doneCount,
    })),
    changes: Array.from({ length: changeCount }, (_, i) => ({
      path: pick(FILE_POOL, seed, i * 2),
      added: 5 + ((seed + i * 7) % 60),
      removed: (seed + i * 3) % 12,
    })),
    decisions: Array.from({ length: 1 + (seed % 3) }, (_, i) => ({
      text: pick(DECISION_POOL, seed, i),
      author: i % 2 === 0 ? "agent" : "you",
    })),
  };
}
