import { Code, ConnectError } from "@connectrpc/connect";
import type { AgentErrorKey } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";

// Apart from api.ts so tests import it without the generated AgentService
// code (whose imports resolve from dashboard/, not installed in islands CI):
// the one import from it is a type, gone at run time.

export const isUnauthenticated = (err: unknown) => ConnectError.from(err).code === Code.Unauthenticated;

// The fleet's error key (proto/agentfleet/v1/errors.proto, mountain plan/15 §4), or "": the ErrorKey detail's
// field 1, read by hand, since the islands do not import the generated code (see above). Both of its fields are
// strings, so each is a varint tag, a varint length and the bytes. A malformed detail reads as "" (the words remain).
export function errorKey(err: unknown): string {
  for (const d of ConnectError.from(err).details) {
    if (!("type" in d) || d.type !== "agentfleet.v1.ErrorKey") continue;
    const b = d.value;
    let i = 0;
    // varint reads one at b[i], at most 5 bytes (2^35 > any length), or -1: by multiplication, never a 32-bit shift
    const varint = () => {
      let v = 0;
      for (let n = 0, scale = 1; n < 5 && i < b.length; n++, scale *= 128) {
        const c = b[i++];
        v += (c & 0x7f) * scale;
        if (c < 0x80) return v;
      }
      return -1;
    };
    while (i < b.length) {
      const tag = varint();
      const len = varint();
      if (tag < 0 || len < 0 || tag % 8 !== 2 || i + len > b.length) break; // not this message's shape
      if (Math.floor(tag / 8) === 1) return new TextDecoder().decode(b.subarray(i, i + len));
      i += len;
    }
  }
  return "";
}

// One text per key AgentService declares (proto/agentfleet/v1/agent.proto's AgentErrorKey), named as the generated
// enum names them: a key added there without text here fails svelte-check. A key's wire string is "agent." and its
// name in lower case (core/internal/errkeys TestDeclared), which keyText reads it by.
type Keyed = Exclude<keyof typeof AgentErrorKey, "UNSPECIFIED">;
const keyText: Record<Keyed, string> = {
  DEFINITION_CHANGED: "This agent was updated. Start a new thread to continue.",
  THREAD_CAP: "You have too many open threads. Close one to start another.",
  THREAD_UNTRUSTED: "This thread holds untrusted input, so only an admin can answer it, from the console.",
  DEFINITION_UNAVAILABLE: "This agent is not available to you.",
};

// The key's text, or "" for a key with none (server.internal, or one a newer core added).
function textOf(key: string): string {
  const name = key.startsWith("agent.") ? key.slice("agent.".length).toUpperCase() : "";
  return Object.hasOwn(keyText, name) ? keyText[name as Keyed] : "";
}

// stale: the thread's agent changed since it opened. The key says so; the words' prefix is read one release longer,
// for a core from before the key.
const stale = (e: ConnectError) => errorKey(e) === "agent.definition_changed" || e.rawMessage.includes("DEFINITION_CHANGED");

// AgentService's refusals (docs/adr/0066, 0067), said plainly: the key's text, else the code's, else the server's
// own message.
export function errorText(err: unknown): string {
  const e = ConnectError.from(err);
  if (stale(e)) return keyText.DEFINITION_CHANGED;
  const keyed = textOf(errorKey(e));
  if (keyed) return keyed;
  switch (e.code) {
    case Code.ResourceExhausted:
      // Unkeyed, it is the warm-time capacity, not the thread cap.
      return "The agent is busy with another session. Try again in a minute.";
    case Code.NotFound:
      return "This thread is no longer available.";
    case Code.PermissionDenied:
      return "You cannot do that in this thread.";
    default:
      return e.rawMessage || "Something went wrong.";
  }
}

export const isStale = (err: unknown) => stale(ConnectError.from(err));

// Someone else answered first: the stream shows their answer.
export const isAlreadyAnswered = (err: unknown) => ConnectError.from(err).code === Code.AlreadyExists;
