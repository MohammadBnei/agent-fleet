#!/usr/bin/env node
// Stdio MCP server wrapping a durable Redis list per task, so the proposer,
// critic, and Discord-relayed human replies can all read the same planning
// transcript. Uses RPUSH + polling LRANGE rather than PUBLISH/SUBSCRIBE:
// pub/sub drops messages published while a reader isn't actively subscribed,
// which is exactly the failure mode a plan-mode approval gate can't afford.
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import Redis from "ioredis";

const redis = new Redis({
  host: process.env.REDIS_HOST ?? "redis.bnei.lan",
  port: Number(process.env.REDIS_PORT ?? 6379),
  password: process.env.REDIS_MAIN_PASSWORD,
});

function planningKey(taskId: string): string {
  return `agentfleet:planning:${taskId}`;
}

async function sendMessage(taskId: string, from: string, text: string) {
  const entry = JSON.stringify({ from, text, ts: new Date().toISOString() });
  const length = await redis.rpush(planningKey(taskId), entry);
  return { index: length - 1 };
}

async function waitForMessages(taskId: string, sinceIndex: number, timeoutMs: number) {
  const deadline = Date.now() + timeoutMs;
  const key = planningKey(taskId);
  while (Date.now() < deadline) {
    const raw = await redis.lrange(key, sinceIndex, -1);
    if (raw.length > 0) {
      return {
        messages: raw.map((r) => JSON.parse(r)),
        nextIndex: sinceIndex + raw.length,
      };
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  return { messages: [], nextIndex: sinceIndex };
}

const server = new Server(
  { name: "agent-fleet-redis", version: "0.1.0" },
  { capabilities: { tools: {} } },
);

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "send_message",
      description:
        "Append a message to this task's shared planning transcript (visible to proposer, critic, and the human via Discord relay).",
      inputSchema: {
        type: "object",
        properties: {
          taskId: { type: "string" },
          from: { type: "string", description: "'proposer' | 'critic' | 'human'" },
          text: { type: "string" },
        },
        required: ["taskId", "from", "text"],
      },
    },
    {
      name: "wait_for_messages",
      description:
        "Block (up to timeoutMs) until new planning-transcript messages appear after sinceIndex, then return them. Never misses messages sent while not waiting.",
      inputSchema: {
        type: "object",
        properties: {
          taskId: { type: "string" },
          sinceIndex: { type: "number", default: 0 },
          timeoutMs: { type: "number", default: 30000 },
        },
        required: ["taskId"],
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args } = request.params;
  const a = args as Record<string, unknown>;
  if (name === "send_message") {
    const result = await sendMessage(a.taskId as string, a.from as string, a.text as string);
    return { content: [{ type: "text", text: JSON.stringify(result) }] };
  }
  if (name === "wait_for_messages") {
    const result = await waitForMessages(
      a.taskId as string,
      Number(a.sinceIndex ?? 0),
      Number(a.timeoutMs ?? 30000),
    );
    return { content: [{ type: "text", text: JSON.stringify(result) }] };
  }
  throw new Error(`Unknown tool: ${name}`);
});

await server.connect(new StdioServerTransport());
