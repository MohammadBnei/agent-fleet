import { Code, ConnectError, createClient, type Client, type Interceptor, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AgentService } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";

// The island is served by core, so the module's own URL names the fleet.
export const fleetOrigin = new URL(import.meta.url).origin;

// docs/adr/0068 §3: the CSRF header is required on every AgentService call,
// GETs included. From a host page it forces a preflight core answers only
// for the origins in embed.origins.
const csrf: Interceptor = (next) => (req) => {
  req.header.set("X-Fleet-Dashboard", "1");
  return next(req);
};

let transport: Transport | undefined;

// The demo page and tests swap in a router transport; nothing else calls it.
export function setTransport(t: Transport) {
  transport = t;
}

export type AgentClient = Client<typeof AgentService>;

export function agentClient(): AgentClient {
  transport ??= createConnectTransport({
    baseUrl: fleetOrigin,
    // The fleet cookie is same-site for *.bnei.dev hosts (§3).
    fetch: ((input: RequestInfo | URL, init?: RequestInit) => fetch(input, { ...init, credentials: "include" })) as typeof fetch,
    interceptors: [csrf],
  });
  return createClient(AgentService, transport);
}

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
      return "You cannot do that in this thread.";
    default:
      return e.rawMessage || "Something went wrong.";
  }
}

// Someone else answered first: the stream shows their answer.
export const isAlreadyAnswered = (err: unknown) => ConnectError.from(err).code === Code.AlreadyExists;
