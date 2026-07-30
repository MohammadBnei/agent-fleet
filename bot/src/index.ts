import {
  Client,
  Events,
  GatewayIntentBits,
  ChannelType,
  SlashCommandBuilder,
  MessageFlags,
} from "discord.js";
import { createTask, findTaskIdByThread, KNOWN_REPOS } from "./db.js";
import { relayHumanMessage } from "./redis.js";
import { log } from "./log.js";

const TRIGGER_CHANNEL_ID = process.env.DISCORD_TRIGGER_CHANNEL_ID;

const client = new Client({
  intents: [
    GatewayIntentBits.Guilds,
    GatewayIntentBits.GuildMessages,
    GatewayIntentBits.MessageContent,
  ],
});

// Kept as a fallback alongside /task, /approve, /stop below — free text
// still works for anyone who forgets the slash command, at zero extra cost.
const TRIGGER_RE = new RegExp(`^!task\\s+(${KNOWN_REPOS.join("|")})\\s*:\\s*(.+)$`, "is");

const commands = [
  new SlashCommandBuilder()
    .setName("task")
    .setDescription("Queue a new agent-fleet task")
    .addStringOption((opt) =>
      opt
        .setName("repo")
        .setDescription("Target repo")
        .setRequired(true)
        .addChoices(...KNOWN_REPOS.map((r) => ({ name: r, value: r }))),
    )
    .addStringOption((opt) =>
      opt.setName("description").setDescription("What should the worker do?").setRequired(true),
    ),
  new SlashCommandBuilder()
    .setName("approve")
    .setDescription("Approve the current plan (use inside a task thread)"),
  new SlashCommandBuilder()
    .setName("stop")
    .setDescription("Cancel this task immediately (use inside a task thread)")
    .addStringOption((opt) => opt.setName("reason").setDescription("Why (optional)")),
];

async function queueTask(
  repo: string,
  description: string,
  channelId: string,
  startThread: (name: string) => Promise<{ id: string; send: (c: string) => Promise<unknown> }>,
): Promise<void> {
  const thread = await startThread(`${repo}: ${description.slice(0, 80)}`);
  const taskId = await createTask(repo, description, channelId, thread.id);
  await thread.send(
    `Queued for **${repo}**. The worker will pick this up shortly and start a proposer/critic planning discussion here — reply to join in, use \`/approve\` once you're happy with the plan, or \`/stop\` to cancel.`,
  );
  log("info", "task created", { taskId, repo, threadId: thread.id });
}

client.once(Events.ClientReady, async (c) => {
  log("info", "logged in", { user: c.user.tag });
  if (!TRIGGER_CHANNEL_ID) return;
  // Guild-scoped registration (not global) so commands show up instantly —
  // global registration can take up to an hour to propagate. Derive the
  // guild from the trigger channel instead of needing a separate config
  // value for it.
  const channel = await c.channels.fetch(TRIGGER_CHANNEL_ID).catch(() => null);
  const guildId = channel && "guildId" in channel ? channel.guildId : undefined;
  if (!guildId) {
    log("error", "could not resolve a guild for trigger channel — slash commands not registered", { channelId: TRIGGER_CHANNEL_ID });
    return;
  }
  try {
    await c.application.commands.set(commands, guildId);
    log("info", "registered slash commands", { guildId });
  } catch (err) {
    // Most likely cause: the bot was invited without the
    // `applications.commands` OAuth2 scope — re-invite it with that scope
    // checked (same client ID; this does not remove it from the server).
    log("error", "failed to register slash commands — was the bot invited with the applications.commands scope?", { error: String(err) });
  }
});

client.on(Events.InteractionCreate, async (interaction) => {
  if (!interaction.isChatInputCommand()) return;

  if (interaction.commandName === "task") {
    const channel = interaction.channel;
    if (interaction.channelId !== TRIGGER_CHANNEL_ID || channel?.type !== ChannelType.GuildText) {
      await interaction.reply({ content: "Use /task in the designated trigger channel.", flags: MessageFlags.Ephemeral });
      return;
    }
    const repo = interaction.options.getString("repo", true);
    const description = interaction.options.getString("description", true);
    await interaction.reply({ content: `Queuing for **${repo}**...`, flags: MessageFlags.Ephemeral });
    await queueTask(repo, description, interaction.channelId, (name) =>
      channel.threads.create({ name, autoArchiveDuration: 1440 }),
    );
    return;
  }

  if (interaction.commandName === "approve" || interaction.commandName === "stop") {
    const taskId = await findTaskIdByThread(interaction.channelId);
    if (!taskId) {
      await interaction.reply({ content: "This isn't a task thread.", flags: MessageFlags.Ephemeral });
      return;
    }
    if (interaction.commandName === "approve") {
      await relayHumanMessage(taskId, "approved", "approve");
      await interaction.reply("Approved.");
    } else {
      const reason = interaction.options.getString("reason") ?? "stop";
      await relayHumanMessage(taskId, reason, "abort");
      await interaction.reply(`Stopping — ${reason}`);
    }
  }
});

client.on(Events.MessageCreate, async (message) => {
  if (message.author.bot) return;

  if (message.channel.id === TRIGGER_CHANNEL_ID && message.channel.type === ChannelType.GuildText) {
    const match = message.content.match(TRIGGER_RE);
    if (!match) return;
    const [, repo, description] = match;
    await queueTask(repo, description, message.channel.id, (name) =>
      message.startThread({ name, autoArchiveDuration: 1440 }),
    );
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
