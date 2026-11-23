import { expect, test } from "bun:test";
import { groupModels, resultRoute, routeOf } from "./modelRoute";

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

test("models group by route, the empty group is left out", () => {
  const all = [
    "claude-opus-5",
    "kimi-k2-7-code",
    "claude-sonnet-5",
    "z-ai-glm-5-3",
  ];
  expect(groupModels(all).map((g) => [g.label, g.models])).toEqual([
    ["Claude (subscription)", ["claude-opus-5", "claude-sonnet-5"]],
    ["Gateway (Venice)", ["kimi-k2-7-code", "z-ai-glm-5-3"]],
  ]);
  expect(groupModels(["claude-opus-5"]).map((g) => g.route)).toEqual([
    "subscription",
  ]);
});
