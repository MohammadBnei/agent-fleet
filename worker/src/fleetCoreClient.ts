// worker's own orchestration code (watchBatch/waitForCheckpointReply) needs
// to read the planning transcript the same way the proposer/critic agent
// sessions do — via fleet-core's wait_for_messages MCP tool, over HTTP.
// This is a raw MCP client for that one purpose; it is NOT a second API —
// same tool, same server, one more caller (see docs/adr/0013). Mirrors
// e2e-provisioner/src/mcpProxy.ts's client-caching shape.
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const FLEET_CORE_URL = process.env.FLEET_CORE_URL ?? "http://fleet-core.agent-fleet.svc.cluster.local:8080";

export type TranscriptMessage = { from: string; text: string; type?: string };

let client: Client | null = null;

async function getClient(): Promise<Client> {
  if (client) return client;
  client = new Client({ name: "agent-fleet-worker", version: "0.1.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`${FLEET_CORE_URL}/mcp`)));
  return client;
}

// readSince mirrors today's redis.lrange(key, cursor, -1): every entry with
// seq >= sinceIndex, plus the next cursor to poll from. Calls the same
// wait_for_messages tool the proposer/critic sessions call, with
// timeoutMs=0 (no long poll) — worker's own watch loops do their own 1s
// sleep between polls, they don't need the server to block too.
export async function readSince(
  taskId: string,
  sinceIndex: number,
): Promise<{ messages: TranscriptMessage[]; nextIndex: number }> {
  const c = await getClient();
  const result = await c.callTool({
    name: "wait_for_messages",
    arguments: { taskId, sinceIndex, timeoutMs: 0 },
  });
  const text = (result.content as { type: string; text: string }[])[0]?.text ?? "{}";
  return JSON.parse(text);
}

// currentCursor mirrors today's redis.llen(planningKey(taskId)) — the
// current transcript length, used to seed the implementation phase's
// abort-watcher cursor so it only sees replies posted after this point.
export async function currentCursor(taskId: string): Promise<number> {
  const { nextIndex } = await readSince(taskId, 0);
  return nextIndex;
}
