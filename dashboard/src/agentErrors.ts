import { ConnectError } from "@connectrpc/connect";
import type { AgentErrorKey } from "./gen/agentfleet/v1/agent_pb";
import { ErrorKeySchema } from "./gen/agentfleet/v1/errors_pb";

// AgentService's keys as the console says them (docs/adr/0081 §3). They reach the console through an event
// proposal, opened through the bridge client's Open (docs/adr/0078 §5), whose refusals arrive keyed. One text per
// key agent.proto's AgentErrorKey declares, named as the generated enum names them, so a key added there without
// text here fails tsc. A key's wire string is "agent." and its name in lower case (core/internal/errkeys
// TestDeclared), which agentErrorText reads it by.
type Keyed = Exclude<keyof typeof AgentErrorKey, "UNSPECIFIED">;
const keyText: Record<Keyed, string> = {
  DEFINITION_CHANGED: "The agent's definition changed since this thread opened. Start a new thread.",
  THREAD_CAP: "This agent's client has too many open threads. Close some, then try again.",
  DEFINITION_UNAVAILABLE: "The bridge may not open this agent's threads: the definition and the client must list each other.",
};

// agentErrorText is the key's text, or the error's own message for a key with none (server.internal, a key a newer
// core added) and for an error without one.
export function agentErrorText(err: unknown): string {
  const e = ConnectError.from(err);
  const key = e.findDetails(ErrorKeySchema)[0]?.key ?? "";
  const name = key.startsWith("agent.") ? key.slice("agent.".length).toUpperCase() : "";
  return Object.hasOwn(keyText, name) ? keyText[name as Keyed] : e.message;
}
