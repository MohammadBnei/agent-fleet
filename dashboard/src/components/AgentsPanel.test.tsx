import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { AgentDefinition } from "../gen/agentfleet/v1/dashboard_pb";
import { AgentRow, appliesOnRestart, modelSource } from "./AgentsPanel";

const def = (over: Partial<AgentDefinition> = {}): AgentDefinition =>
  ({
    name: "alert-responder",
    valid: true,
    error: "",
    model: "z-ai-glm-5-3",
    yamlModel: "z-ai-glm-5-3",
    overrideModel: "",
    overrideBy: "",
    overrideAt: "",
    overrideIgnored: "",
    choices: [
      { model: "z-ai-glm-5-3", unavailableReason: "" },
      { model: "claude-opus-5", unavailableReason: "not in model.allow" },
    ],
    standing: false,
    running: false,
    runningModel: "",
    ...over,
  }) as AgentDefinition;

test("the source says where the model comes from", () => {
  expect(modelSource(def())).toBe("yaml");
  expect(modelSource(def({ yamlModel: "", model: "" }))).toBe(
    "Claude Code's pick",
  );
  expect(
    modelSource(
      def({
        overrideModel: "z-ai-glm-5-3",
        overrideBy: "root",
        overrideAt: "2026-11-24T10:00:00Z",
      }),
    ),
  ).toBe("override by root 2026-11-24");
  // An ignored override is not the source.
  expect(
    modelSource(def({ overrideModel: "x", overrideIgnored: "gone" })),
  ).toBe("yaml");
});

test("only allowed choices are selectable, unavailable ones carry their reason", () => {
  const html = renderToStaticMarkup(
    <AgentRow d={def()} busy={false} onPick={() => {}} />,
  );
  expect(html).toContain('<option value="z-ai-glm-5-3">');
  expect(html).not.toContain('<option value="claude-opus-5">');
  expect(html).toContain("not in model.allow");
});

test("an ignored override is shown with its reason", () => {
  const html = renderToStaticMarkup(
    <AgentRow
      d={def({
        overrideModel: "claude-opus-5",
        overrideIgnored: "not allowed",
      })}
      busy={false}
      onPick={() => {}}
    />,
  );
  expect(html).toContain("claude-opus-5 is ignored: not allowed");
});

test("a running standing session says a change applies on restart", () => {
  const d = def({ standing: true, running: true, runningModel: "" });
  expect(appliesOnRestart(d)).toBe(true);
  expect(
    appliesOnRestart(
      def({ standing: true, running: true, runningModel: "z-ai-glm-5-3" }),
    ),
  ).toBe(false);
  expect(appliesOnRestart(def({ standing: true, running: false }))).toBe(false);
  expect(
    renderToStaticMarkup(<AgentRow d={d} busy={false} onPick={() => {}} />),
  ).toContain("applies on restart");
});
