import { describe, expect, it, beforeAll, afterAll } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { rtkRewrite, rtkRunCommandHook } from "./rtkHook.js";

// A stand-in for the real binary: same contract (PreToolUse payload on
// stdin, an updatedInput answer or nothing on stdout), no rtk install
// needed in CI.
let dir: string;
let fakeRtk: string;

function stub(body: string): string {
  const path = join(dir, `rtk-${Math.random().toString(36).slice(2)}`);
  writeFileSync(path, `#!/bin/sh\n${body}\n`);
  chmodSync(path, 0o755);
  return path;
}

beforeAll(() => {
  // session.test.ts sets RTK_DISABLED, and `bun test` runs every file in
  // one process — without this the tests below silently pass through
  // whenever that file happens to be imported first.
  delete process.env.RTK_DISABLED;
  dir = mkdtempSync(join(tmpdir(), "rtk-hook-"));
  fakeRtk = stub(
    `cat > /dev/null; echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk go test ./..."}}}'`,
  );
});
afterAll(() => rmSync(dir, { recursive: true, force: true }));

describe("rtkRewrite", () => {
  it("swaps in the rewritten command", async () => {
    expect(await rtkRewrite("Bash", { command: "go test ./...", description: "test" }, fakeRtk)).toEqual({
      command: "rtk go test ./...",
    });
  });

  it("leaves the command alone when rtk says nothing", async () => {
    const input = { command: "echo hi" };
    expect(await rtkRewrite("Bash", input, stub("cat > /dev/null"))).toBe(input);
  });

  it("leaves the command alone when rtk is missing or broken", async () => {
    const input = { command: "go test ./..." };
    expect(await rtkRewrite("Bash", input, join(dir, "no-such-binary"))).toBe(input);
    expect(await rtkRewrite("Bash", input, stub("echo not-json"))).toBe(input);
  });

  it("ignores a tool call with no command at all", async () => {
    const input = { file_path: "/tmp/x" };
    expect(await rtkRewrite("Read", input, fakeRtk)).toBe(input);
  });
});

describe("rtkRunCommandHook", () => {
  it("allows the call, since the SDK drops updatedInput otherwise", async () => {
    process.env.RTK_BIN = fakeRtk;
    try {
      const out = (await rtkRunCommandHook(
        {
          hook_event_name: "PreToolUse",
          tool_name: "mcp__agent-fleet-sidecar__run_command",
          tool_input: { command: "go test ./..." },
        } as never,
        undefined,
        { signal: new AbortController().signal },
      )) as { hookSpecificOutput?: Record<string, unknown> };
      // permissionDecision "allow" is the part that silently regresses: the
      // SDK throws the rewrite away without it.
      expect(out.hookSpecificOutput).toEqual({
        hookEventName: "PreToolUse",
        permissionDecision: "allow",
        permissionDecisionReason: "rtk output compaction",
        updatedInput: { command: "rtk go test ./..." },
      });
    } finally {
      delete process.env.RTK_BIN;
    }
  });

  it("stays out of the way when there is nothing to rewrite", async () => {
    const out = await rtkRunCommandHook({ hook_event_name: "PreToolUse", tool_name: "Bash" } as never, undefined, {
      signal: new AbortController().signal,
    });
    expect(out).toEqual({});
  });
});

describe("opt-outs", () => {
  it("RTK_DISABLED leaves every command alone", async () => {
    process.env.RTK_DISABLED = "1";
    try {
      const input = { command: "go test ./..." };
      expect(await rtkRewrite("Bash", input, fakeRtk)).toBe(input);
    } finally {
      delete process.env.RTK_DISABLED;
    }
  });
});
