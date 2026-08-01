// One MCP server per task, reachable at /mcp/:taskId — this is the single,
// stable endpoint the worker's Agent SDK session talks to for the whole
// implementation phase (see worker/src/planning.ts's e2eMcpServer()). It
// exposes request_e2e_env/kill_env directly, and once a pod is live for
// that task, also whatever Playwright tools mcpProxy.ts reports — so the
// session's tool *set* changes over time instead of the session needing a
// new MCP server mid-conversation (unsupported in the SDK's stable API).
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import { createSession, getActiveSessionForTask, getTask, requestKill, setSessionStatus } from "./db.js";
import { createE2ePod, previewUrlFor } from "./k8s.js";
import { proxiedTools, proxyCallTool } from "./mcpProxy.js";
import { log } from "./log.js";

const PORT = Number(process.env.PORT ?? 8080);

const LOCAL_TOOLS = [
  {
    name: "request_e2e_env",
    description:
      "Request an on-demand e2e test environment for this task: a live pod running the app on this branch, code-server for human review, and a Playwright MCP server. Returns the preview URL. Safe to call again if one is already running — returns the existing session's URL.",
    inputSchema: { type: "object", properties: {} },
  },
  {
    name: "kill_env",
    description: "Tear down this task's e2e environment now, without waiting for the task to finish.",
    inputSchema: { type: "object", properties: {} },
  },
];

async function requestE2eEnv(taskId: string): Promise<{ url: string }> {
  const existing = await getActiveSessionForTask(taskId);
  if (existing) return { url: previewUrlFor(taskId) };

  const task = await getTask(taskId);
  if (!task) throw new Error(`unknown task ${taskId}`);

  const session = await createSession(taskId);
  try {
    const { podName, ingressPath } = await createE2ePod(task);
    await setSessionStatus(session.id, "running", { pod_name: podName, ingress_path: ingressPath });
    return { url: previewUrlFor(taskId) };
  } catch (err) {
    await setSessionStatus(session.id, "failed");
    throw err;
  }
}

function serverFor(taskId: string): Server {
  const server = new Server({ name: "agent-fleet-e2e", version: "0.1.0" }, { capabilities: { tools: {} } });

  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: [...LOCAL_TOOLS, ...(await proxiedTools(taskId))],
  }));

  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const { name, arguments: args } = request.params;
    if (name === "request_e2e_env") {
      const result = await requestE2eEnv(taskId);
      // Lets the Agent SDK know the tool list changed so it re-fetches and
      // sees the Playwright tools that just became available — verify this
      // actually happens in practice (see docs/adr/0012's risks).
      await server.notification({ method: "notifications/tools/list_changed" });
      return { content: [{ type: "text", text: JSON.stringify(result) }] };
    }
    if (name === "kill_env") {
      await requestKill(taskId);
      return { content: [{ type: "text", text: "kill requested" }] };
    }
    const result = await proxyCallTool(taskId, name, (args as Record<string, unknown>) ?? {});
    return result as { content: unknown[] };
  });

  return server;
}

const transports = new Map<string, StreamableHTTPServerTransport>();

async function handleMcpRequest(taskId: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
  let transport = transports.get(taskId);
  if (!transport) {
    transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await serverFor(taskId).connect(transport);
    transports.set(taskId, transport);
  }
  await transport.handleRequest(req, res);
}

export function startHttpServer(): void {
  const server = createServer((req, res) => {
    const match = req.url?.match(/^\/mcp\/([^/?]+)/);
    if (!match) {
      res.writeHead(404).end();
      return;
    }
    const taskId = match[1];
    handleMcpRequest(taskId, req, res).catch((err) => {
      log("error", "mcp request failed", { taskId, error: String(err) });
      if (!res.headersSent) res.writeHead(500).end();
    });
  });
  server.listen(PORT, () => log("info", "e2e-provisioner http server listening", { port: PORT }));
}
