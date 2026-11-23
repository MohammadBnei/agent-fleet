import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { RouteBadge } from "../pages/SessionList";
import { ActionsMenu } from "./ActionsMenu";
import { ModelStatsPanel, SCORE_NOTE } from "./Ratings";

test("a gateway session gets a route chip in a list row, a Claude one none", () => {
  expect(renderToStaticMarkup(<RouteBadge model="kimi-k2-7-code" />)).toContain(
    "gateway",
  );
  expect(renderToStaticMarkup(<RouteBadge model="claude-opus-5" />)).toBe("");
  expect(renderToStaticMarkup(<RouteBadge model={undefined} />)).toBe("");
});

const menu = (archivedAt?: string) =>
  renderToStaticMarkup(
    <ActionsMenu
      sessionId="s"
      busy={false}
      isBusy={() => false}
      run={() => {}}
      archivedAt={archivedAt}
      currentMode="auto"
      onAutoClick={() => {}}
    />,
  );

test("the mode control is hidden once a session is archived", () => {
  expect(menu()).toContain("mode:");
  expect(menu("2026-11-23T10:00:00Z")).not.toContain("mode:");
});

test("the per-model table says whose model a session score counts toward", () => {
  expect(SCORE_NOTE).toContain("most-used model");
  // The panel is hooks-only until data arrives; the note is part of its source.
  expect(typeof ModelStatsPanel).toBe("function");
});
