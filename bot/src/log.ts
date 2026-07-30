// Tiny structured logger — one JSON object per line to stdout, matching
// what Alloy/Loki expect for LogQL field filtering instead of parsing plain
// text. Duplicated from worker/src/log.ts (same ~5 lines) — not worth a
// shared internal package for this size, see that file's comment.
export function log(level: "info" | "error", message: string, fields: Record<string, unknown> = {}): void {
  console.log(JSON.stringify({ level, message, ts: new Date().toISOString(), ...fields }));
}
