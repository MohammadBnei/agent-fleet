import { Redis } from "ioredis";

const redis = new Redis({
  host: process.env.REDIS_HOST ?? "redis.bnei.lan",
  port: Number(process.env.REDIS_PORT ?? 6379),
  password: process.env.REDIS_MAIN_PASSWORD,
});

// Same key/shape as mcp-redis's send_message tool — the bot writes directly
// with ioredis instead of going through MCP (it isn't a Claude Code agent).
// `type` distinguishes an explicit /approve or /stop slash command from
// ordinary discussion text — planning.ts checks `type` first (unambiguous)
// and falls back to matching words in `text` for anyone who just types
// "approved" instead of using the command.
export async function relayHumanMessage(
  taskId: string,
  text: string,
  type: "discussion" | "approve" | "abort" = "discussion",
): Promise<void> {
  await redis.rpush(
    `agentfleet:planning:${taskId}`,
    JSON.stringify({ from: "human", text, type, ts: new Date().toISOString() }),
  );
}
