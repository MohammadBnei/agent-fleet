import { test, expect } from "bun:test";
import { log } from "./log.js";

test("LOG_LEVEL filters out lower-severity lines", () => {
  const lines: string[] = [];
  const origLog = console.log;
  const origLevel = process.env.LOG_LEVEL;
  console.log = (s: string) => lines.push(s);
  process.env.LOG_LEVEL = "warn";
  try {
    log("debug", "should be filtered");
    log("info", "should be filtered");
    log("warn", "should print");
    log("error", "should print", { taskId: "t1" });
  } finally {
    console.log = origLog;
    process.env.LOG_LEVEL = origLevel;
  }
  expect(lines).toHaveLength(2);
  const parsed = lines.map((l) => JSON.parse(l));
  expect(parsed[0]).toMatchObject({ level: "WARN", msg: "should print" });
  expect(parsed[1]).toMatchObject({ level: "ERROR", msg: "should print", taskId: "t1" });
  expect(parsed[1]).toHaveProperty("time");
});
