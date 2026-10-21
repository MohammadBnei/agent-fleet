import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";

const importRe = /^\s*(?:import|export)\b[^;]*?\bfrom\s+["']([^"']+)["']/gm;

// docs/adr/0068 §1: the island talks to AgentService only. It may share the
// generated messages and plain transcript logic, never the admin API or the
// console's components.
const allowed = new Set([
  "../../dashboard/src/gen/agentfleet/v1/agent_pb",
  "../../dashboard/src/gen/agentfleet/v1/transcript_pb",
  "../../dashboard/src/transcript",
]);

test("island imports stay off DashboardService and the console", () => {
  const bad: string[] = [];
  for (const f of readdirSync(new URL(".", import.meta.url))) {
    if (!/\.(ts|svelte)$/.test(f) || f.endsWith(".test.ts")) continue;
    const src = readFileSync(new URL(f, import.meta.url), "utf8");
    for (const [, spec] of src.matchAll(importRe)) {
      if (spec.includes("dashboard") && !allowed.has(spec)) bad.push(`${f}: ${spec}`);
    }
  }
  expect(bad).toEqual([]);
});

test("dashboard/src/transcript.ts imports nothing but generated messages", () => {
  const src = readFileSync(new URL("../../dashboard/src/transcript.ts", import.meta.url), "utf8");
  const specs = [...src.matchAll(importRe)].map((m) => m[1]);
  expect(specs.filter((s) => !s.startsWith("./gen/agentfleet/v1/") || s.includes("dashboard_pb"))).toEqual([]);
});
