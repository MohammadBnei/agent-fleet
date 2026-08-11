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

// core's log viewer scopes a task with `| json | taskId="..."`. Without this
// field the worker's lines carry nothing tying them to a task, and the only
// alternative is parsing the pod name — which means duplicating the
// provisioner's shortID() truncation rule into core.
test("every line carries taskId from TASK_ID", () => {
  const lines: string[] = [];
  const origLog = console.log;
  const origTask = process.env.TASK_ID;
  console.log = (s: string) => lines.push(s);
  process.env.TASK_ID = "task-from-env";
  try {
    log("info", "no explicit fields");
    log("info", "explicit taskId wins", { taskId: "explicit" });
  } finally {
    console.log = origLog;
    process.env.TASK_ID = origTask;
  }
  const parsed = lines.map((l) => JSON.parse(l));
  expect(parsed[0].taskId).toBe("task-from-env");
  // Several call sites already pass a taskId of their own; those must not be
  // clobbered by the ambient one.
  expect(parsed[1].taskId).toBe("explicit");
});

test("taskId is omitted entirely when TASK_ID is unset", () => {
  const lines: string[] = [];
  const origLog = console.log;
  const origTask = process.env.TASK_ID;
  console.log = (s: string) => lines.push(s);
  delete process.env.TASK_ID;
  try {
    log("info", "no task context");
  } finally {
    console.log = origLog;
    if (origTask !== undefined) process.env.TASK_ID = origTask;
  }
  expect(JSON.parse(lines[0])).not.toHaveProperty("taskId");
});
