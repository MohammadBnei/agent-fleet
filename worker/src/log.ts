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

export function log(level: Level, message: string, fields: Record<string, unknown> = {}): void {
  if (LEVELS[level] < LEVELS[currentLevel()]) return;
  console.log(JSON.stringify({ time: new Date().toISOString(), level: level.toUpperCase(), msg: message, ...fields }));
}
