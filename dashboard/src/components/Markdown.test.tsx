import { test, expect } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { Markdown } from "./Markdown";

// renderToStaticMarkup rather than a DOM: this app has no jsdom/happy-dom and
// needs none here — the question is only what markup react-markdown produces.

test("a GFM pipe table renders as a real table, not a wall of pipes", () => {
  const html = renderToStaticMarkup(
    <Markdown text={"| tool | count |\n|---|---|\n| Bash | 237 |\n"} />,
  );
  expect(html).toContain("<table");
  expect(html).toContain("<th");
  expect(html).toContain("Bash");
  expect(html).toContain("237");
  // The wrapper is what stops a wide table widening the feed column.
  expect(html).toContain("overflow-x-auto");
  // The literal source must not survive as text — that was the bug.
  expect(html).not.toContain("|---|");
});

test("fenced code still owns its own markup after the pre unwrap", () => {
  const html = renderToStaticMarkup(<Markdown text={"```sh\nls -la\n```"} />);
  expect(html).toContain("<pre");
  expect(html).toContain("ls -la");
});

test("a human message's blockquote still renders", () => {
  const html = renderToStaticMarkup(<Markdown text="> hello" />);
  expect(html).toContain("<blockquote");
});
