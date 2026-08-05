import type { Task } from "./gen/agentfleet/v1/dashboard_pb";

// ponytail: the "herd" mock design shows several fields the real backend
// doesn't expose at all — no proto field, no schema column, nothing to
// wire up later short of a real backend change (unlike needsYou/branch/
// changes/todos, which used to live here too and are now real — todos
// comes from the transcript's TodoWrite tool calls, see transcript.ts's
// latestTodos). This file fakes only what's left, deterministically per
// task id (so a value doesn't jitter every 5s poll):
//   - decisions: no backend representation of a decision log exists.
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
  decisions: { text: string; author: string }[];
};

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
  const idleMin = 1 + (seed % 40);

  return {
    idleLabel: `idle ${idleMin}m`,
    currentActivity: pick(ACTIVITY_POOL, seed, 0),
    model: pick(MODELS, seed, 0),
    tokens: `${((seed % 900) / 10 + 5).toFixed(1)}k tokens`,
    startedAt: `${9 + (seed % 8)}:${(seed % 6) * 10 || "00"}`,
    decisions: Array.from({ length: 1 + (seed % 3) }, (_, i) => ({
      text: pick(DECISION_POOL, seed, i),
      author: i % 2 === 0 ? "agent" : "you",
    })),
  };
}
