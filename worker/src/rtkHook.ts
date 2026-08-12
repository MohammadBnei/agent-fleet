import type { HookCallback } from "@anthropic-ai/claude-agent-sdk";

// rtk (Rust Token Killer) rewrites a shell command into its own compacting
// proxy (`go test ./...` → `rtk go test ./...`, ~99% less output). It ships
// as a Claude Code PreToolUse hook; worker/Dockerfile installs the binary
// and fleet-shared/settings.json declares the hook.
//
// That declaration never fired. **The Agent SDK does not run hooks declared
// in settings files, even with settingSources: ["user"]** — verified
// 2026-08-13 against the local ~/.claude/settings.json, which carries the
// same rtk hook: `rtk gain --history` recorded nothing for a command the SDK
// ran, and a hit for the identical command run through the CLI. Only
// `options.hooks` callbacks run, so the hook is registered from session.ts.
//
// `rtk hook claude` reads a PreToolUse payload on stdin and answers with an
// `updatedInput` holding the rewritten command, or with nothing at all for a
// command it doesn't compress. It only ever looks at `tool_input.command`,
// so the same call works for the sidecar's `run_command` as for `Bash`.
// Anything unexpected — binary missing, non-JSON output — leaves the command
// untouched; a token optimization must never fail a tool call.
export async function rtkRewrite(
  toolName: string,
  toolInput: Record<string, unknown>,
  bin = process.env.RTK_BIN ?? "rtk",
): Promise<Record<string, unknown>> {
  // Two opt-outs. RTK_DISABLED is the fleet-wide kill switch (a pod env
  // change, no rebuild) for when compaction is hiding something and every
  // command needs its raw output. Per-command, the agent prefixes `rtk
  // proxy ` — rtk's own passthrough, which its hook deliberately leaves
  // alone; fleet-shared/CLAUDE.md tells the agent so.
  if (process.env.RTK_DISABLED) return toolInput;
  if (typeof toolInput.command !== "string") return toolInput;
  try {
    const proc = Bun.spawn([bin, "hook", "claude"], {
      stdin: new TextEncoder().encode(
        JSON.stringify({ hook_event_name: "PreToolUse", tool_name: toolName, tool_input: toolInput }),
      ),
      stdout: "pipe",
      stderr: "ignore",
    });
    const out = (await new Response(proc.stdout).text()).trim();
    if (!out) return toolInput;
    const updated = JSON.parse(out)?.hookSpecificOutput?.updatedInput;
    return updated && typeof updated.command === "string" ? updated : toolInput;
  } catch {
    return toolInput;
  }
}

// Registered for the sidecar's `run_command` only — the e2e sandbox, where
// the builds and test runs (the fleet's real token sink) happen.
//
// `permissionDecision: "allow"` is not optional here: the SDK drops a hook's
// `updatedInput` entirely unless the hook also allows the call (verified the
// same day — with "ask", or with no decision at all, the original command is
// what runs). That is exactly why this hook is scoped to `run_command` and
// never to `Bash`: `run_command` is already un-prompted by design
// (docs/adr/0039), so allowing it here changes nothing, whereas doing the
// same for `Bash` would silently bypass the human permission gate
// docs/adr/0029 locks in. Bash gets its rewrite from canUseTool instead,
// after the human has answered.
export const rtkRunCommandHook: HookCallback = async (input) => {
  const hookInput = input as unknown as { tool_name?: string; tool_input?: Record<string, unknown> };
  const toolInput = hookInput.tool_input;
  if (!toolInput) return {};
  const updatedInput = await rtkRewrite(hookInput.tool_name ?? "", toolInput);
  if (updatedInput === toolInput) return {};
  return {
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "allow",
      permissionDecisionReason: "rtk output compaction",
      updatedInput,
    },
  };
};
