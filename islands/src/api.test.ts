import { expect, test } from "bun:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { errorText, isAlreadyAnswered } from "./api";

// AgentService's refusals (infra-bootstrap-9f, A2/A3) read as plain words.
test("refusals map to plain words", () => {
  expect(errorText(new ConnectError("DEFINITION_CHANGED: start a new thread", Code.FailedPrecondition))).toContain("Start a new thread");
  expect(errorText(new ConnectError("pool full", Code.ResourceExhausted))).toContain("busy");
  expect(errorText(new ConnectError("no", Code.NotFound))).toContain("no longer available");
  expect(errorText(new ConnectError("answers_json must be an object", Code.InvalidArgument))).toBe("answers_json must be an object");
  expect(isAlreadyAnswered(new ConnectError("answered", Code.AlreadyExists))).toBe(true);
});
