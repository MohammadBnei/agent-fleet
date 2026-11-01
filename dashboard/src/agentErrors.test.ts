import { expect, test } from "bun:test";
import { create, toBinary } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { agentErrorText, errorText } from "./agentErrors";
import { ErrorKeySchema } from "./gen/agentfleet/v1/errors_pb";

// keyed is a ConnectError carrying the ErrorKey detail as core sends it: its type name and its bytes.
function keyed(msg: string, code: Code, key: string): ConnectError {
  const e = new ConnectError(msg, code);
  e.details.push({
    type: "agentfleet.v1.ErrorKey",
    value: toBinary(ErrorKeySchema, create(ErrorKeySchema, { key })),
  });
  return e;
}

test("a key reads as its text; anything else as the error's message", () => {
  expect(
    agentErrorText(
      keyed(
        "this client has 500 open threads; close some first",
        Code.ResourceExhausted,
        "agent.thread_cap",
      ),
    ),
  ).toContain("too many open threads");
  expect(
    agentErrorText(
      keyed(
        "unknown definition",
        Code.NotFound,
        "agent.definition_unavailable",
      ),
    ),
  ).toContain("may not open");
  expect(agentErrorText(keyed("busy", Code.Aborted, "server.internal"))).toBe(
    "[aborted] busy",
  );
  expect(
    agentErrorText(keyed("new", Code.NotFound, "agent.something_new")),
  ).toBe("[not_found] new");
  expect(agentErrorText(new ConnectError("plain", Code.Internal))).toBe(
    "[internal] plain",
  );
  // A decision answered twice: the card shows the key's text and the id core logged the refusal under.
  const twice = keyed("stale", Code.FailedPrecondition, "agent.decision_stale");
  twice.metadata.set("Trace-Id", "2862b494c9c106a97920f1530cdcceea");
  expect(agentErrorText(twice)).toBe(
    "This card was replaced by a newer one.\n\nTrace ID: 2862b494c9c106a97920f1530cdcceea",
  );
  // An unkeyed refusal (no such decision) keeps its message and gains the id.
  expect(
    agentErrorText(
      new ConnectError(
        "no such decision",
        Code.NotFound,
        new Headers({ "Trace-Id": "abc" }),
      ),
    ),
  ).toBe("[not_found] no such decision\n\nTrace ID: abc");
});

test("errorText adds the response's trace id to a ConnectError, and leaves anything else alone", () => {
  for (const [name, err, want] of [
    [
      "with a trace id",
      new ConnectError(
        "boom",
        Code.Internal,
        new Headers({ "Trace-Id": "4bf92f3577b34da6a3ce929d0e0e4736" }),
      ),
      "[internal] boom\n\nTrace ID: 4bf92f3577b34da6a3ce929d0e0e4736",
    ],
    ["without one", new ConnectError("boom", Code.Internal), "[internal] boom"],
    ["a plain error", new Error("network down"), "network down"],
  ] as const) {
    expect(errorText(err), name).toBe(want);
  }
});
