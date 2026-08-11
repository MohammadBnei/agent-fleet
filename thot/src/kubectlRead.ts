/**
 * A read-only kubectl surface thot can use WITHOUT a human prompt.
 *
 * This is the escape hatch ADR-0035's implementation notes reserved for
 * exactly this moment: "if the clicking becomes painful, the fix is a
 * narrow read-only MCP tool — NOT putting Bash in allowedTools, which
 * would un-gate mutation too."
 *
 * Read freely, mutate under a gate. `kubectl_read` is listed in
 * allowedTools (so it bypasses canUseTool entirely); plain Bash is not,
 * so `kubectl rollout restart` / `delete` still block on a human.
 *
 * Three independent layers keep this read-only, because the RBAC layer
 * alone is NOT sufficient — thot's ClusterRole deliberately grants
 * `patch` on deployments and `delete` on pods, so a "read-only" tool that
 * merely forwarded arbitrary args could mutate:
 *
 *   1. Verb allowlist — the first argument must be a known read verb.
 *   2. Flag denylist — no credential/endpoint/impersonation overrides.
 *   3. No shell, ever — argv is passed as an array to spawn(), so shell
 *      metacharacters are inert by construction. `get pods; delete pod x`
 *      cannot split into two commands because nothing parses it.
 */
import { tool } from "@anthropic-ai/claude-agent-sdk";
import { z } from "zod";

// Every kubectl subcommand that only reads. Deliberately excludes some
// read-ish ones (`rollout` has a mutating `restart` subcommand; `auth
// can-i` is fine but `auth reconcile` writes) — when in doubt it's left
// out, since Bash-under-a-gate remains available for anything else.
const READ_VERBS = new Set([
  "get",
  "describe",
  "logs",
  "top",
  "events",
  "explain",
  "api-resources",
  "api-versions",
  "cluster-info",
  "version",
  "diff",
]);

// Flags that would redirect who or what we're talking to. `--as`/`--as-group`
// matter even though thot has no `impersonate` RBAC verb: defense in depth
// costs one line here, and RBAC is not the layer this tool should lean on.
const DENIED_FLAG_PREFIXES = [
  "--as",
  "--as-group",
  "--as-uid",
  "--kubeconfig",
  "--server",
  "--token",
  "--client-key",
  "--client-certificate",
  "--username",
  "--password",
];

export function validateArgs(args: string[]): string | null {
  if (args.length === 0) return "no arguments given";

  const verb = args[0];
  if (!READ_VERBS.has(verb)) {
    return `'${verb}' is not a read-only verb. Allowed: ${[...READ_VERBS].sort().join(", ")}. Use Bash for anything that mutates — it will ask a human first.`;
  }

  for (const arg of args) {
    // Compare on the part before '=' so --as=admin is caught alongside
    // the space-separated form.
    const flag = arg.split("=", 1)[0];
    if (DENIED_FLAG_PREFIXES.includes(flag)) {
      return `flag '${flag}' is not permitted`;
    }
  }
  return null;
}

const MAX_OUTPUT = 100_000;

export async function runKubectlRead(args: string[]): Promise<string> {
  const invalid = validateArgs(args);
  if (invalid) return `refused: ${invalid}`;

  // Array argv, never a shell string — this is what makes injection
  // structurally impossible rather than merely filtered.
  const proc = Bun.spawn(["kubectl", ...args], {
    stdout: "pipe",
    stderr: "pipe",
  });

  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);

  const body = (stdout + (stderr ? `\n[stderr]\n${stderr}` : "")).trim();
  const clipped =
    body.length > MAX_OUTPUT
      ? body.slice(0, MAX_OUTPUT) + `\n… (truncated at ${MAX_OUTPUT} chars)`
      : body;

  return exitCode === 0 ? clipped || "(no output)" : `kubectl exited ${exitCode}\n${clipped}`;
}

export const kubectlReadTool = tool(
  "kubectl_read",
  "Run a READ-ONLY kubectl command against ukubi-cluster. No human approval needed, so prefer this for all diagnosis. Pass argv as an array, e.g. ['get','pods','-A','-o','wide'] or ['logs','deploy/core','-n','agent-fleet','--tail','100']. Only read verbs are accepted (get, describe, logs, top, events, explain, api-resources, api-versions, cluster-info, version, diff). Anything that mutates must go through Bash instead, which will ask a human first.",
  {
    args: z
      .array(z.string())
      .min(1)
      .describe("kubectl arguments as an array, without the leading 'kubectl'."),
  },
  async ({ args }) => ({
    content: [{ type: "text" as const, text: await runKubectlRead(args) }],
  }),
);
