import { Code, ConnectError } from "@connectrpc/connect";

// Apart from api.ts so tests import it without the generated AgentService
// code (whose imports resolve from dashboard/, not installed in islands CI).

export const isUnauthenticated = (err: unknown) => ConnectError.from(err).code === Code.Unauthenticated;

// The fleet's error key (proto/agentfleet/v1/errors.proto, mountain plan/15 §4), or "": the ErrorKey detail's
// field 1, read by hand, since the islands do not import the generated code (see above). Both of its fields are
// strings, so each is a tag, a varint length and the bytes.
export function errorKey(err: unknown): string {
  for (const d of ConnectError.from(err).details) {
    if (!("type" in d) || d.type !== "agentfleet.v1.ErrorKey") continue;
    const b = d.value;
    let i = 0;
    while (i < b.length) {
      const tag = b[i++];
      let len = 0;
      for (let shift = 0; i < b.length; shift += 7) {
        const c = b[i++];
        len |= (c & 0x7f) << shift;
        if (c < 0x80) break;
      }
      if ((tag & 7) !== 2) break; // not a string: not this message's shape
      if (tag >> 3 === 1) return new TextDecoder().decode(b.subarray(i, i + len));
      i += len;
    }
  }
  return "";
}

// stale: the thread's agent changed since it opened. The key says so; the words' prefix is read one release longer,
// for a core from before the key.
const stale = (e: ConnectError) => errorKey(e) === "agent.definition_changed" || e.rawMessage.includes("DEFINITION_CHANGED");

// AgentService's refusals (docs/adr/0066, 0067), said plainly. The server's
// own message stays the fallback.
export function errorText(err: unknown): string {
  const e = ConnectError.from(err);
  if (stale(e)) return "This agent was updated. Start a new thread to continue.";
  switch (e.code) {
    case Code.ResourceExhausted:
      return "The agent is busy with another session. Try again in a minute.";
    case Code.NotFound:
      return "This thread is no longer available.";
    case Code.PermissionDenied:
      return "You cannot do that in this thread. If it holds untrusted input, only an admin can answer it, from the console.";
    default:
      return e.rawMessage || "Something went wrong.";
  }
}

export const isStale = (err: unknown) => stale(ConnectError.from(err));

// Someone else answered first: the stream shows their answer.
export const isAlreadyAnswered = (err: unknown) => ConnectError.from(err).code === Code.AlreadyExists;
