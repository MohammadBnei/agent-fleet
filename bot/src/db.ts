import pg from "pg";

const pool = new pg.Pool({
  host: process.env.AGENTFLEET_DB_HOST ?? "postgres.bnei.lan",
  port: Number(process.env.AGENTFLEET_DB_PORT ?? 5432),
  database: process.env.AGENTFLEET_DB_NAME ?? "agentfleetdb",
  user: process.env.AGENTFLEET_DB_USER ?? "dbuser_agentfleet",
  password: process.env.AGENTFLEET_DB_PASSWORD,
});

export const KNOWN_REPOS = ["dream-analyst", "vos-monolith"];

export async function createTask(
  repo: string,
  description: string,
  channelId: string,
  threadId: string,
): Promise<string> {
  const { rows } = await pool.query(
    `INSERT INTO tasks (repo, description, discord_channel_id, discord_thread_id)
     VALUES ($1, $2, $3, $4) RETURNING id`,
    [repo, description, channelId, threadId],
  );
  return rows[0].id as string;
}

export async function findTaskIdByThread(threadId: string): Promise<string | null> {
  const { rows } = await pool.query(`SELECT id FROM tasks WHERE discord_thread_id = $1`, [
    threadId,
  ]);
  return rows[0]?.id ?? null;
}
