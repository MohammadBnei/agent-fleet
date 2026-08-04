// Tiny structured logger — one JSON object per line to stdout, matching
// what Alloy/Loki expect for LogQL field filtering instead of parsing plain
// text. Only 4 call sites in this package, so a real logging dependency
// (pino etc.) would be more code than it saves; duplicated as-is in bot/src
// for the same reason — not worth a shared internal package for this size.
export function log(level: "info" | "warn" | "error", message: string, fields: Record<string, unknown> = {}): void {
  console.log(JSON.stringify({ level, message, ts: new Date().toISOString(), ...fields }));
}
