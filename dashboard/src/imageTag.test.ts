import { expect, test } from "bun:test";
import { imageTag } from "./imageTag";

test("takes the tag off a normal ref", () => {
  expect(imageTag("mohammaddocker/agent-fleet-worker:3.5.4")).toBe("3.5.4");
  expect(imageTag("agent-fleet-worker:latest")).toBe("latest");
});

// A registry port is not a tag. Getting this wrong labels every cell "5000".
test("a registry port is not a tag", () => {
  expect(imageTag("reg:5000/agent-fleet-worker")).toBe("reg:5000/agent-fleet-worker");
  expect(imageTag("reg:5000/agent-fleet-worker:3.5.4")).toBe("3.5.4");
});

test("an untagged or empty ref is returned as-is", () => {
  expect(imageTag("agent-fleet-worker")).toBe("agent-fleet-worker");
  expect(imageTag("")).toBe("");
});
