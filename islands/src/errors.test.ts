import { expect, test } from "bun:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorKey, errorText, isAlreadyAnswered, isStale } from "./errors";

// AgentService's refusals (infra-bootstrap-9f, A2/A3) read as plain words.
test("refusals map to plain words", () => {
  expect(errorText(new ConnectError("DEFINITION_CHANGED: start a new thread", Code.FailedPrecondition))).toContain("Start a new thread");
  expect(errorText(new ConnectError("pool full", Code.ResourceExhausted))).toContain("busy");
  expect(errorText(new ConnectError("no", Code.NotFound))).toContain("no longer available");
  expect(errorText(new ConnectError("answers_json must be an object", Code.InvalidArgument))).toBe("answers_json must be an object");
  expect(isAlreadyAnswered(new ConnectError("answered", Code.AlreadyExists))).toBe(true);
});

// keyed is a ConnectError carrying an ErrorKey detail as it arrives from core (its wire bytes).
function keyed(msg: string, code: Code, key: string, source = ""): ConnectError {
  const enc = new TextEncoder();
  const field = (n: number, v: string) => { const b = enc.encode(v); return [(n << 3) | 2, b.length, ...b]; };
  const e = new ConnectError(msg, code);
  e.details.push({ type: "agentfleet.v1.ErrorKey", value: new Uint8Array([...field(1, key), ...(source ? field(2, source) : [])]) });
  return e;
}

// The key, read from the detail, decides; the words stay the fallback one release.
test("the error key", () => {
  expect(errorKey(keyed("start a new thread", Code.FailedPrecondition, "agent.definition_changed"))).toBe("agent.definition_changed");
  expect(errorKey(keyed("x", Code.Internal, "server.internal", "store"))).toBe("server.internal");
  expect(errorKey(new ConnectError("no detail", Code.Internal))).toBe("");
  const other = new ConnectError("x", Code.Internal);
  other.details.push({ type: "google.rpc.ErrorInfo", value: new Uint8Array([10, 1, 120]) });
  expect(errorKey(other)).toBe("");
  // keyed, with words a new core may change: still stale
  expect(isStale(keyed("start a new thread", Code.FailedPrecondition, "agent.definition_changed"))).toBe(true);
  expect(errorText(keyed("start a new thread", Code.FailedPrecondition, "agent.definition_changed"))).toContain("Start a new thread");
  // an older core: the words alone
  expect(isStale(new ConnectError("DEFINITION_CHANGED: start a new thread", Code.FailedPrecondition))).toBe(true);
  // another precondition is not stale (the positive control)
  expect(isStale(keyed("gone", Code.FailedPrecondition, "server.internal"))).toBe(false);
});
