import { test, expect, describe } from "bun:test";
import { validateArgs, runKubectlRead } from "./kubectlRead.js";

describe("read verbs are accepted", () => {
  for (const args of [
    ["get", "pods", "-A"],
    ["describe", "deploy/core", "-n", "agent-fleet"],
    ["logs", "deploy/core", "--tail", "100"],
    ["events", "-n", "thot"],
    ["top", "nodes"],
    ["api-resources"],
  ]) {
    test(args.join(" "), () => {
      expect(validateArgs(args)).toBeNull();
    });
  }
});

// thot's ClusterRole really does grant `patch` on deployments and `delete`
// on pods, so this tool cannot lean on RBAC to stay read-only — these are
// the checks that actually enforce it.
describe("mutating verbs are refused", () => {
  for (const args of [
    ["delete", "pod", "core-abc"],
    ["patch", "deploy/core", "-p", "{}"],
    ["apply", "-f", "manifest.yaml"],
    ["edit", "deploy/core"],
    ["scale", "deploy/core", "--replicas", "0"],
    ["rollout", "restart", "deploy/core"],
    ["exec", "-it", "pod/core", "--", "sh"],
    ["port-forward", "svc/core", "8080:8080"],
    ["cp", "pod:/etc/passwd", "/tmp/x"],
    ["drain", "k8s-worker-01"],
  ]) {
    test(args.join(" "), () => {
      expect(validateArgs(args)).toContain("not a read-only verb");
    });
  }
});

describe("credential and impersonation overrides are refused", () => {
  for (const args of [
    ["get", "pods", "--as", "system:admin"],
    ["get", "pods", "--as=system:admin"],
    ["get", "pods", "--as-group=system:masters"],
    ["get", "secrets", "--kubeconfig", "/tmp/evil.conf"],
    ["get", "pods", "--server=https://evil.example.com"],
    ["get", "pods", "--token=abc"],
  ]) {
    test(args.join(" "), () => {
      expect(validateArgs(args)).toContain("not permitted");
    });
  }
});

test("empty args are refused", () => {
  expect(validateArgs([])).toBe("no arguments given");
});

// The load-bearing structural guarantee: argv is an array passed straight
// to spawn(), never a shell string, so metacharacters are inert. A
// "command" smuggled into one argument is just a nonsense verb.
test("shell metacharacters cannot chain a second command", () => {
  expect(validateArgs(["get pods; kubectl delete pod core"])).toContain("not a read-only verb");
  expect(validateArgs(["get", "pods", "&&", "kubectl", "delete", "pod", "x"])).toBeNull();
  // ^ that one validates (the verb is `get`), but "&&" reaches kubectl as
  // a literal argument rather than a shell operator — kubectl rejects it,
  // and no second process is ever spawned.
});

test("a refusal never shells out", async () => {
  const out = await runKubectlRead(["delete", "pod", "anything"]);
  expect(out).toStartWith("refused:");
});
