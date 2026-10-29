import { expect, test } from "bun:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorKey, errorText, isAlreadyAnswered, isStale } from "./errors";

// AgentService's refusals (infra-bootstrap-9f, A2/A3) read as plain words.
test("refusals map to plain words", () => {
  expect(
    errorText(
      new ConnectError(
        "DEFINITION_CHANGED: start a new thread",
        Code.FailedPrecondition,
      ),
    ),
  ).toContain("Start a new thread");
  expect(
    errorText(new ConnectError("pool full", Code.ResourceExhausted)),
  ).toContain("busy");
  expect(errorText(new ConnectError("no", Code.NotFound))).toContain(
    "no longer available",
  );
  expect(
    errorText(
      new ConnectError("answers_json must be an object", Code.InvalidArgument),
    ),
  ).toBe("answers_json must be an object");
  expect(
    isAlreadyAnswered(new ConnectError("answered", Code.AlreadyExists)),
  ).toBe(true);
});

// keyed is a ConnectError carrying an ErrorKey detail as it arrives from core (its wire bytes).
function keyed(
  msg: string,
  code: Code,
  key: string,
  source = "",
): ConnectError {
  const enc = new TextEncoder();
  const field = (n: number, v: string) => {
    const b = enc.encode(v);
    return [(n << 3) | 2, b.length, ...b];
  };
  const e = new ConnectError(msg, code);
  e.details.push({
    type: "agentfleet.v1.ErrorKey",
    value: new Uint8Array([
      ...field(1, key),
      ...(source ? field(2, source) : []),
    ]),
  });
  return e;
}

// The key, read from the detail, decides; the words stay the fallback one release.
test("the error key", () => {
  expect(
    errorKey(
      keyed(
        "start a new thread",
        Code.FailedPrecondition,
        "agent.definition_changed",
      ),
    ),
  ).toBe("agent.definition_changed");
  expect(errorKey(keyed("x", Code.Internal, "server.internal", "store"))).toBe(
    "server.internal",
  );
  expect(errorKey(new ConnectError("no detail", Code.Internal))).toBe("");
  const other = new ConnectError("x", Code.Internal);
  other.details.push({
    type: "google.rpc.ErrorInfo",
    value: new Uint8Array([10, 1, 120]),
  });
  expect(errorKey(other)).toBe("");
  // keyed, with words a new core may change: still stale
  expect(
    isStale(
      keyed(
        "start a new thread",
        Code.FailedPrecondition,
        "agent.definition_changed",
      ),
    ),
  ).toBe(true);
  expect(
    errorText(
      keyed(
        "start a new thread",
        Code.FailedPrecondition,
        "agent.definition_changed",
      ),
    ),
  ).toContain("Start a new thread");
  // an older core: the words alone
  expect(
    isStale(
      new ConnectError(
        "DEFINITION_CHANGED: start a new thread",
        Code.FailedPrecondition,
      ),
    ),
  ).toBe(true);
  // another precondition is not stale (the positive control)
  expect(
    isStale(keyed("gone", Code.FailedPrecondition, "server.internal")),
  ).toBe(false);
});

// raw is a ConnectError whose ErrorKey detail is the given bytes.
function raw(bytes: number[]): ConnectError {
  const e = new ConnectError("x", Code.Internal);
  e.details.push({
    type: "agentfleet.v1.ErrorKey",
    value: new Uint8Array(bytes),
  });
  return e;
}

// The decoder's edges: a field order Go never writes, a length over 127, and malformed bytes, which read as "" and
// never loop (a length whose 32-bit shift went negative once moved the cursor backwards forever).
test("the error key's bytes", () => {
  const enc = new TextEncoder();
  const k = [...enc.encode("agent.definition_changed")];
  expect(
    errorKey(raw([0x12, 5, ...enc.encode("store"), 0x0a, k.length, ...k])),
  ).toBe("agent.definition_changed");
  const long = "a".repeat(200);
  expect(errorKey(raw([0x0a, 0xc8, 0x01, ...enc.encode(long)]))).toBe(long);
  expect(errorKey(raw([0x12, 0xfa, 0xff, 0xff, 0xff, 0x0f]))).toBe(""); // a length past the buffer
  expect(errorKey(raw([0x12, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01]))).toBe(""); // a varint past 5 bytes
  expect(errorKey(raw([0x0a, 10, 0x61]))).toBe(""); // truncated
  expect(errorKey(raw([0x0a]))).toBe(""); // no length
  expect(errorKey(raw([0x08, 0x01]))).toBe(""); // a varint field: not this shape
});

// A code that means several things reads by its key (docs/adr/0081 §2); unkeyed, or a key with no text, the code's.
test("keyed refusals", () => {
  expect(
    errorText(
      keyed(
        "you have 50 open threads; close one first",
        Code.ResourceExhausted,
        "agent.thread_cap",
      ),
    ),
  ).toContain("too many open threads");
  expect(
    errorText(
      keyed(
        "unknown definition",
        Code.NotFound,
        "agent.definition_unavailable",
      ),
    ),
  ).toBe("This agent is not available to you.");
  // unkeyed: the code's words, which name no cause
  expect(
    errorText(
      keyed(
        "you cannot post to this thread",
        Code.PermissionDenied,
        "server.internal",
      ),
    ),
  ).toBe("You cannot do that in this thread.");
  expect(
    errorText(keyed("at capacity", Code.ResourceExhausted, "server.internal")),
  ).toContain("busy");
  // a key a newer core added, and a name that is no key: the code's words
  expect(errorText(keyed("gone", Code.NotFound, "agent.something_new"))).toBe(
    "This thread is no longer available.",
  );
  expect(errorText(keyed("gone", Code.NotFound, "agent.constructor"))).toBe(
    "This thread is no longer available.",
  );
});
