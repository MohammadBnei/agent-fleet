import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import type { ModelStat } from "../gen/agentfleet/v1/dashboard_pb";
import {
  ModelStatsTable,
  SessionScore,
  statRow,
  TurnRating,
} from "./Ratings";

const stat = (over: Partial<ModelStat> = {}): ModelStat =>
  ({
    model: "zai-org-glm-5",
    route: "gateway",
    sessions: 3n,
    inputTokens: 1200n,
    outputTokens: 300n,
    billedUsd: 0.5,
    listUsd: 0.5,
    thumbsUp: 2n,
    thumbsDown: 1n,
    avgScore: 4.25,
    scores: 2n,
    errorResults: 1n,
    ...over,
  }) as ModelStat;

test("stat row shows avg score only when scored", () => {
  expect(statRow(stat())[8]).toBe("4.3");
  expect(statRow(stat({ scores: 0n }))[8]).toBe("—");
  expect(statRow(stat())[5]).toBe("$0.50");
});

test("table renders a row per model and an empty note", () => {
  const html = renderToStaticMarkup(<ModelStatsTable stats={[stat()]} />);
  expect(html).toContain("zai-org-glm-5");
  expect(html).toContain("avg score");
  expect(renderToStaticMarkup(<ModelStatsTable stats={[]} />)).toContain(
    "No sessions",
  );
});

test("rating controls render unpressed", () => {
  const t = renderToStaticMarkup(<TurnRating sessionId="s" seq={4n} />);
  expect(t).toContain('aria-pressed="false"');
  const s = renderToStaticMarkup(<SessionScore sessionId="s" />);
  expect(s).toContain(">5<");
  expect(s).not.toContain("note (optional)");
});
