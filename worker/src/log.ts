// Tiny structured logger — one JSON object per line to stdout, matching
// what Alloy/Loki expect for LogQL field filtering instead of parsing plain
// text. Only a handful of call sites in this package, so a real logging
// dependency (pino etc.) would be more code than it saves; duplicated as-is
// in bot/src for the same reason — not worth a shared internal package for
// this size. Field names (time/level/msg) deliberately match Go's
// slog.NewJSONHandler default output shape, so LogQL queries work the same
// across the fleet's Go and TS services.
const LEVELS = { debug: 0, info: 1, warn: 2, error: 3 } as const;
type Level = keyof typeof LEVELS;

function currentLevel(): Level {
  const envLevel = process.env.LOG_LEVEL as Level | undefined;
  return envLevel && envLevel in LEVELS ? envLevel : "info";
}

// taskId is stamped onto every line, mirroring the sidecar's
// slog.Default().With("taskId", ...). Without it a worker's logs carry
// nothing tying them to a task, so core's log viewer had no way to scope a
// query except by parsing the pod name — which meant duplicating the
// provisioner's shortID() truncation rule into core. One field here removes
// that coupling: every fleet component is filterable with the same
// `| json | taskId="..."`. Read per-call rather than cached at import so
// tests can set it after loading this module.
//
// Explicit `fields` still wins: several call sites already pass a taskId
// (their own, or another task's), and those must not be clobbered.
export function log(level: Level, message: string, fields: Record<string, unknown> = {}): void {
  if (LEVELS[level] < LEVELS[currentLevel()]) return;
  const sessionId = process.env.SESSION_ID;
  console.log(
    JSON.stringify({
      time: new Date().toISOString(),
      level: level.toUpperCase(),
      msg: message,
      ...(sessionId ? { sessionId } : {}),
      ...fields,
    }),
  );
}
