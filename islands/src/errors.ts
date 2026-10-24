import { Code, ConnectError } from "@connectrpc/connect";

// Apart from api.ts so tests import it without the generated AgentService
// code (whose imports resolve from dashboard/, not installed in islands CI).

export const isUnauthenticated = (err: unknown) => ConnectError.from(err).code === Code.Unauthenticated;

// AgentService's refusals (docs/adr/0066, 0067), said plainly. The server's
// own message stays the fallback.
export function errorText(err: unknown): string {
  const e = ConnectError.from(err);
  if (e.rawMessage.includes("DEFINITION_CHANGED")) return "This agent was updated. Start a new thread to continue.";
  switch (e.code) {
    case Code.ResourceExhausted:
      return "The agent is busy with another session. Try again in a minute.";
    case Code.NotFound:
      return "This thread is no longer available.";
    case Code.PermissionDenied:
      return "You cannot do that in this thread. It may be locked, or hold untrusted input only an admin can answer.";
    default:
      return e.rawMessage || "Something went wrong.";
  }
}

export const isStale = (err: unknown) => ConnectError.from(err).rawMessage.includes("DEFINITION_CHANGED");

// Someone else answered first: the stream shows their answer.
export const isAlreadyAnswered = (err: unknown) => ConnectError.from(err).code === Code.AlreadyExists;
