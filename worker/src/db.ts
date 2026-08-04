import pg from "pg";

const pool = new pg.Pool({
  host: process.env.AGENTFLEET_DB_HOST ?? "postgres.bnei.lan",
  port: Number(process.env.AGENTFLEET_DB_PORT ?? 5432),
  database: process.env.AGENTFLEET_DB_NAME ?? "agentfleetdb",
  user: process.env.AGENTFLEET_DB_USER ?? "dbuser_agentfleet",
  password: process.env.AGENTFLEET_DB_PASSWORD,
});

export interface Task {
  id: string;
  repo: string;
  description: string;
  status: string;
  discord_channel_id: string;
  discord_thread_id: string | null;
  claimed_by: string | null;
  pr_url: string | null;
  planning_session_id: string | null;
  retry_count: number;
  last_error: string | null;
  heartbeat_at: string | null;
  lease_id: string | null;
}

// Atomic claim: SKIP LOCKED means two worker pods for the same repo (or a
// restarted pod racing its predecessor) never double-claim the same task.
// Also reclaims tasks stuck in claimed/planning/implementing whose heartbeat
// has gone stale (crash recovery — see docs/adr/0016). `status` is
// deliberately preserved (not overwritten) on reclaim: the CASE only
// promotes a fresh 'pending' row to 'claimed', so the caller can still tell
// which phase the task died in ('implementing' = real resume candidate).
// heartbeat_at IS NULL matches too, so pre-migration in-flight rows are
// reclaimable instead of permanent zombies. lease_id is re-minted on every
// claim/reclaim so stillHoldsLease can detect a superseded claim before an
// irreversible action.
export async function claimNextTask(repo: string, workerName: string): Promise<Task | null> {
  const { rows } = await pool.query(
    `UPDATE tasks SET
       claimed_by = $2,
       heartbeat_at = now(),
       updated_at = now(),
       lease_id = gen_random_uuid(),
       status = CASE WHEN status = 'pending' THEN 'claimed' ELSE status END,
       retry_count = CASE WHEN status = 'pending' THEN retry_count ELSE retry_count + 1 END,
       last_error = CASE WHEN status = 'pending' THEN last_error
                         ELSE 'reclaimed: previous claimant heartbeat stale (was ' || claimed_by || ')' END
     WHERE id = (
       SELECT id FROM tasks
       WHERE repo = $1 AND (
         status = 'pending'
         OR (status IN ('claimed', 'planning', 'implementing')
             AND (heartbeat_at IS NULL OR heartbeat_at < now() - interval '10 minutes'))
       )
       ORDER BY created_at
       FOR UPDATE SKIP LOCKED
       LIMIT 1
     )
     RETURNING *`,
    [repo, workerName],
  );
  return rows[0] ?? null;
}

export async function updateHeartbeat(id: string): Promise<void> {
  await pool.query(`UPDATE tasks SET heartbeat_at = now() WHERE id = $1`, [id]);
}

export async function saveSessionIds(id: string, planningSessionId: string | null): Promise<void> {
  await pool.query(`UPDATE tasks SET planning_session_id = $2 WHERE id = $1`, [id, planningSessionId]);
}

// Requeues a task after a self-detected transient error instead of failing
// it outright. Returns false (and leaves the task untouched) once
// retry_count has already hit the cap, so the caller falls through to its
// normal terminal-failure path instead of retrying forever.
export async function requeueTransient(id: string, error: string, maxRetries: number): Promise<boolean> {
  const { rows } = await pool.query(
    `UPDATE tasks SET status = 'pending', claimed_by = NULL, retry_count = retry_count + 1,
       last_error = $2, updated_at = now()
     WHERE id = $1 AND retry_count < $3
     RETURNING id`,
    [id, error, maxRetries],
  );
  return rows.length > 0;
}

// Called immediately before every irreversible action (push/PR, worktree
// removal) so a worker whose heartbeat went stale during a network
// partition — not an actual crash — aborts instead of racing whoever
// reclaimed the row. See docs/adr/0016.
export async function stillHoldsLease(id: string, leaseId: string): Promise<boolean> {
  const { rows } = await pool.query(`SELECT 1 FROM tasks WHERE id = $1 AND lease_id = $2`, [id, leaseId]);
  return rows.length > 0;
}

export async function setTaskStatus(
  id: string,
  status: string,
  fields: Partial<Pick<Task, "pr_url">> = {},
): Promise<void> {
  await pool.query(
    `UPDATE tasks SET status = $2, pr_url = COALESCE($3, pr_url), updated_at = now() WHERE id = $1`,
    [id, status, fields.pr_url ?? null],
  );
}

export async function appendJournal(
  repo: string | null,
  actor: string,
  eventType: string,
  payload: Record<string, unknown> = {},
): Promise<void> {
  await pool.query(
    `INSERT INTO knowledge_journal (repo, actor, event_type, payload) VALUES ($1, $2, $3, $4)`,
    [repo, actor, eventType, JSON.stringify(payload)],
  );
}
