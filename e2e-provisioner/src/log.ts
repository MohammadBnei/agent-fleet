// Same tiny structured logger as worker/src/log.ts and bot/src/log.ts —
// duplicated rather than shared for four call sites; see worker's own
// comment for why a real logging dependency isn't worth it at this size.
export function log(level: "info" | "error", message: string, fields: Record<string, unknown> = {}): void {
  console.log(JSON.stringify({ level, message, ts: new Date().toISOString(), ...fields }));
}
