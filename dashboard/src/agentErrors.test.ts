import { expect, test } from "bun:test";
import { create, toBinary } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { agentErrorText } from "./agentErrors";
import { ErrorKeySchema } from "./gen/agentfleet/v1/errors_pb";

// keyed is a ConnectError carrying the ErrorKey detail as core sends it: its type name and its bytes.
function keyed(msg: string, code: Code, key: string): ConnectError {
  const e = new ConnectError(msg, code);
  e.details.push({ type: "agentfleet.v1.ErrorKey", value: toBinary(ErrorKeySchema, create(ErrorKeySchema, { key })) });
  return e;
}

test("a key reads as its text; anything else as the error's message", () => {
  expect(agentErrorText(keyed("this client has 500 open threads; close some first", Code.ResourceExhausted, "agent.thread_cap"))).toContain("too many open threads");
  expect(agentErrorText(keyed("unknown definition", Code.NotFound, "agent.definition_unavailable"))).toContain("may not open");
  expect(agentErrorText(keyed("busy", Code.Aborted, "server.internal"))).toBe("[aborted] busy");
  expect(agentErrorText(keyed("new", Code.NotFound, "agent.something_new"))).toBe("[not_found] new");
  expect(agentErrorText(new ConnectError("plain", Code.Internal))).toBe("[internal] plain");
});
