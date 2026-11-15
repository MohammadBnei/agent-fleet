import { expect, test } from "bun:test";
import { SpanRecorder, type WireSpan } from "./spans.js";

const tokens = (input: number) => ({
  input,
  output: 0,
  cacheRead: 0,
  cacheCreation: 0,
});

function recorder() {
  const batches: WireSpan[][] = [];
  const rec = new SpanRecorder(async (b) => {
    batches.push(b);
  });
  return { rec, batches };
}

const turnAttrs = (b: WireSpan[]) =>
  (b.find((s) => s.name === "invoke_agent") as WireSpan).attrs;

test("the turn span leads its batch, however many tool spans follow", async () => {
  const { rec, batches } = recorder();
  for (let i = 0; i < 600; i++) {
    rec.toolStart(`T${i}`, "Bash");
    rec.toolEnd(`T${i}`, false);
  }
  rec.endTurn({ uuid: "u1", totalCostUsd: 0.1, tokens: tokens(10) });
  await rec.flush();
  expect(batches[0]).toHaveLength(601);
  expect(batches[0][0].name).toBe("invoke_agent");
});

test("a zeroed error result neither reports usage nor moves the baseline", async () => {
  const { rec, batches } = recorder();
  rec.endTurn({ uuid: "a", totalCostUsd: 0.1, tokens: tokens(100) });
  rec.endTurn({
    uuid: "b",
    isError: true,
    totalCostUsd: 0,
    tokens: tokens(0),
  });
  rec.endTurn({ uuid: "c", totalCostUsd: 0.25, tokens: tokens(150) });
  await rec.flush();
  expect(turnAttrs(batches[1])["gen_ai.usage.cost_micro_usd"]).toBeUndefined();
  // Not 250000 / 150: the zero in between did not reset the baseline.
  expect(turnAttrs(batches[2])["gen_ai.usage.cost_micro_usd"]).toBe(150_000);
  expect(turnAttrs(batches[2])["gen_ai.usage.input_tokens"]).toBe(50);
});

test("a lower cost total is a new process: every field restarts from zero", async () => {
  const { rec, batches } = recorder();
  rec.endTurn({ uuid: "a", totalCostUsd: 0.5, tokens: tokens(500) });
  rec.endTurn({ uuid: "b", totalCostUsd: 0.1, tokens: tokens(200) });
  await rec.flush();
  expect(turnAttrs(batches[1])["gen_ai.usage.cost_micro_usd"]).toBe(100_000);
  expect(turnAttrs(batches[1])["gen_ai.usage.input_tokens"]).toBe(200);
});
