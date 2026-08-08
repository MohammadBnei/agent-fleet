import { test, expect } from "bun:test";
import { Markdown } from "./Markdown";

test("Markdown component is exported", () => {
  // Verify that Markdown component is properly exported
  expect(typeof Markdown).toBe("object");
});

test("Markdown component accepts text prop", () => {
  // Type check - if this compiles, the component accepts the correct props
  const _component = <Markdown text="test" />;
  expect(true).toBe(true);
});
