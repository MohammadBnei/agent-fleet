import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { ErrorMessage } from "./ErrorMessage";

test("the trace id is selectable whole and has a copy button", () => {
  const html = renderToStaticMarkup(
    <ErrorMessage message={"boom\n\nTrace ID: 28a59a2d"} />,
  );
  expect(html).toContain('<span class="select-all break-all">28a59a2d</span>');
  expect(html).toContain('aria-label="Copy trace ID"');
  expect(html).not.toContain("\n\nTrace ID");
});

test("a message without a trace id renders as it was", () => {
  expect(renderToStaticMarkup(<ErrorMessage message="network down" />)).toBe(
    "network down",
  );
});
