import Redis from "ioredis";

const redis = new Redis({
  host: process.env.REDIS_HOST ?? "redis.bnei.lan",
  port: Number(process.env.REDIS_PORT ?? 6379),
  password: process.env.REDIS_MAIN_PASSWORD,
});

// Same key/shape as mcp-redis's send_message tool — the bot writes directly
// with ioredis instead of going through MCP (it isn't a Claude Code agent).
export async function relayHumanMessage(taskId: string, text: string): Promise<void> {
  await redis.rpush(
    `agentfleet:planning:${taskId}`,
    JSON.stringify({ from: "human", text, ts: new Date().toISOString() }),
  );
}
