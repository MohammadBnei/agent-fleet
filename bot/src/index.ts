import { Client, Events, GatewayIntentBits, ChannelType } from "discord.js";
import { createTask, findTaskIdByThread, KNOWN_REPOS } from "./db.js";
import { relayHumanMessage } from "./redis.js";

const TRIGGER_CHANNEL_ID = process.env.DISCORD_TRIGGER_CHANNEL_ID;

const client = new Client({
  intents: [
    GatewayIntentBits.Guilds,
    GatewayIntentBits.GuildMessages,
    GatewayIntentBits.MessageContent,
  ],
});

// Trigger shape: "!task <repo>: <description>" posted in the designated
// channel. Human picks the repo and description; the worker for that repo
// polls Postgres and picks the task up.
const TRIGGER_RE = new RegExp(`^!task\\s+(${KNOWN_REPOS.join("|")})\\s*:\\s*(.+)$`, "is");

client.on(Events.MessageCreate, async (message) => {
  if (message.author.bot) return;

  if (message.channel.id === TRIGGER_CHANNEL_ID && message.channel.type === ChannelType.GuildText) {
    const match = message.content.match(TRIGGER_RE);
    if (!match) return;
    const [, repo, description] = match;
    const thread = await message.startThread({
      name: `${repo}: ${description.slice(0, 80)}`,
      autoArchiveDuration: 1440,
    });
    const taskId = await createTask(repo, description, message.channel.id, thread.id);
    await thread.send(
      `Queued for **${repo}**. The worker will pick this up shortly and start a proposer/critic planning discussion here — reply in this thread to join in, and say "approved" once you're happy with the plan.`,
    );
    console.log(`created task ${taskId} (${repo}) in thread ${thread.id}`);
    return;
  }

  if (message.channel.isThread()) {
    const taskId = await findTaskIdByThread(message.channel.id);
    if (taskId) {
      await relayHumanMessage(taskId, message.content);
    }
  }
});

client.login(process.env.DISCORD_BOT_TOKEN);
