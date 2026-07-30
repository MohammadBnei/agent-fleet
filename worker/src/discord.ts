const BOT_TOKEN = process.env.DISCORD_BOT_TOKEN;

export async function postReply(threadOrChannelId: string, content: string): Promise<void> {
  const chunks = chunk(content, 1900);
  for (const c of chunks) {
    const res = await fetch(`https://discord.com/api/v10/channels/${threadOrChannelId}/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bot ${BOT_TOKEN}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ content: c }),
    });
    if (!res.ok) {
      throw new Error(`Discord post failed: ${res.status} ${await res.text()}`);
    }
  }
}

function chunk(text: string, size: number): string[] {
  const out: string[] = [];
  for (let i = 0; i < text.length; i += size) out.push(text.slice(i, i + size));
  return out.length ? out : [""];
}
