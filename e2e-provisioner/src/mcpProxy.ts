// Proxies Playwright MCP tool calls to whichever e2e pod is currently live
// for a task. The worker's Agent SDK session never connects to the
// ephemeral pod directly (its address isn't known until after
// request_e2e_env returns, and the SDK sets mcpServers once per query()
// call) — it always talks to this provisioner's stable /mcp/:taskId
// endpoint, which forwards here.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { playwrightUrlFor } from "./k8s.js";
import { log } from "./log.js";

const clients = new Map<string, Client>();

async function clientFor(taskId: string): Promise<Client> {
  const existing = clients.get(taskId);
  if (existing) return existing;

  const client = new Client({ name: "agent-fleet-e2e-provisioner", version: "0.1.0" });
  // Confirm this path against the installed @playwright/mcp version once a
  // real e2e-runner image exists — see e2e-runner/entrypoint.sh's comment.
  const transport = new StreamableHTTPClientTransport(new URL(playwrightUrlFor(taskId)));
  await client.connect(transport);
  clients.set(taskId, client);
  return client;
}

export function dropClient(taskId: string): void {
  const client = clients.get(taskId);
  clients.delete(taskId);
  client?.close().catch((err) => log("error", "failed closing playwright mcp client", { taskId, error: String(err) }));
}

// Returns [] rather than throwing when no e2e pod is live yet (or it hasn't
// finished starting) — http.ts merges this into tools/list, so "no
// Playwright tools yet" is the normal pre-request_e2e_env state, not an
// error.
export async function proxiedTools(taskId: string): Promise<{ name: string; description?: string; inputSchema: unknown }[]> {
  try {
    const client = await clientFor(taskId);
    const { tools } = await client.listTools();
    return tools;
  } catch (err) {
    log("info", "no playwright mcp tools available yet", { taskId, error: String(err) });
    return [];
  }
}

export async function proxyCallTool(taskId: string, name: string, args: Record<string, unknown>): Promise<unknown> {
  const client = await clientFor(taskId);
  return client.callTool({ name, arguments: args });
}
