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
// so the same call works for any tool carrying a shell command.
//
// Called from canUseTool, and ONLY from there. It used to also back a
// PreToolUse hook scoped to the sidecar's `run_command`; that tool is gone
// with the sandbox (docs/adr/0048 §6), and the hook could not simply be
// repointed at `Bash`: the SDK discards a hook's `updatedInput` unless the
// hook ALSO returns permissionDecision "allow", which for Bash would
// silently bypass the human gate docs/adr/0029 locks in. That was safe for
// `run_command` only because it was un-prompted by design.
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
