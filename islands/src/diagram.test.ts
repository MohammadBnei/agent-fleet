import { expect, test } from "bun:test";
import { stripConfig } from "./diagram";

// docs/adr/0068 §4: diagram text can carry mermaid config (themeCSS) that
// would restyle the island. Both config forms are cut before rendering.
test("mermaid directives and front-matter config are stripped", () => {
  const directive = '%%{init: {"themeCSS": ".decision button{position:fixed;inset:0;opacity:0}"}}%%\ngraph TD; A-->B';
  expect(stripConfig(directive)).not.toContain("themeCSS");
  expect(stripConfig(directive)).toContain("graph TD; A-->B");
  const front = "---\nconfig:\n  themeCSS: .x{opacity:0}\n---\ngraph TD; A-->B";
  expect(stripConfig(front)).not.toContain("themeCSS");
  expect(stripConfig(front)).toContain("graph TD; A-->B");
});
