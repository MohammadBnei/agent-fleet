import { expect, test } from "bun:test";
import { resultRoute, routeOf } from "./modelRoute";

test("route follows the claude- prefix", () => {
  expect(routeOf("claude-opus-4-7")).toBe("subscription");
  expect(routeOf("zai-org-glm-5")).toBe("gateway");
  expect(routeOf("")).toBe("");
  expect(routeOf(undefined)).toBe("");
});

test("a stamped route beats the model id", () => {
  expect(resultRoute({ route: "gateway", model: "claude-x" })).toBe("gateway");
  expect(resultRoute({ model: "kimi-k2-7-code" })).toBe("gateway");
});
