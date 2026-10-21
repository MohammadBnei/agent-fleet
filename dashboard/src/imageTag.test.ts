import { expect, test } from "bun:test";
import { imageTag, podVersion } from "./imageTag";

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

test("podVersion names the release and keeps the src tag beside it", () => {
  const ref = "reg:5000/agent-fleet-worker:src-8a19aa40c3d2";
  expect(podVersion("6.22.1", ref)).toBe("6.22.1 (src-8a19aa40)");
  expect(podVersion("dev", ref)).toBe("dev (src-8a19aa40)");
  // A pod built before the provisioner reported its release.
  expect(podVersion(undefined, ref)).toBe("src-8a19aa40");
  expect(podVersion("", "agent-fleet-worker:3.5.4")).toBe("3.5.4");
  expect(podVersion("6.22.1", "")).toBe("6.22.1");
});
