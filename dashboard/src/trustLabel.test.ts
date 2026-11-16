import { expect, test } from "bun:test";
import { trustLabel } from "./trustLabel";

test("trustLabel", () => {
  expect(trustLabel("BC")).toBe("B·C");
  expect(trustLabel("AB")).toBe("A·B");
  expect(trustLabel("AC")).toBe("A·C");
  expect(trustLabel("ABC")).toBe("supervised");
  expect(trustLabel("")).toBe("");
});
