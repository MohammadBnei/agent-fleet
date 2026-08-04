// Replaces worker/src/db.ts (direct Postgres) and worker/src/
// fleetCoreClient.ts (direct remote MCP client) — as of docs/adr/0020, core
// is the fleet's sole Postgres-credential holder and the worker talks only
// to its own pod's localhost sidecar, over the sidecar's plain HTTP/JSON
// wrapper-facing API (not MCP, not gRPC — see sidecar/internal/localapi's
// own header comment for why). The agent's own MCP tool calls (send_message,
// wait_for_messages, AskUserQuestion, request_e2e_env, kill_env, Playwright
// tools) go through the SDK's own mcpServers config pointed at the
// sidecar's separate MCP port instead — this file is only for the wrapper's
// own control-flow/housekeeping calls (docs/adr/0020 point 5's third
// responsibility).
const SIDECAR_API_ADDR = process.env.SIDECAR_API_ADDR ?? "localhost:9091";
const base = `http://${SIDECAR_API_ADDR}`;

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`${base}${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`sidecar ${path} failed: ${res.status} ${await res.text()}`);
  return res.json() as Promise<T>;
}

export async function heartbeat(leaseId: string): Promise<void> {
  await postJSON("/heartbeat", { leaseId });
}

export async function setStatus(
  status: string,
  fields: { prUrl?: string | null; notes?: string | null; lastError?: string | null } = {},
): Promise<void> {
  await postJSON("/status", {
    status,
    prUrl: fields.prUrl ?? null,
    notes: fields.notes ?? null,
    lastError: fields.lastError ?? null,
  });
}

export async function appendJournal(
  repo: string,
  actor: string,
  eventType: string,
  payload: Record<string, unknown> = {},
): Promise<void> {
  await postJSON("/journal", { repo, actor, eventType, payload });
}

export async function saveSessionId(planningSessionId: string, model: string): Promise<void> {
  await postJSON("/session-id", { planningSessionId, model });
}

export async function stillHoldsLease(leaseId: string): Promise<boolean> {
  const res = await fetch(`${base}/still-holds-lease?leaseId=${encodeURIComponent(leaseId)}`);
  if (!res.ok) throw new Error(`sidecar still-holds-lease failed: ${res.status}`);
  const body = (await res.json()) as { holds: boolean };
  return Boolean(body.holds);
}

// pushMessage lets the wrapper post directly into the transcript — the
// round-cap checkpoint text, and the agent's raw assistant narration (today
// relayed to Discord by the now-deleted discord.ts; both now route through
// core's own relay loop, uniformly with the agent's own send_message posts).
export async function pushMessage(from: string, text: string, type?: string): Promise<void> {
  await postJSON("/message", { from, text, type });
}

export type TranscriptEntry = { seq: number; from: string; text: string; type?: string };

// streamHumanMessages consumes the sidecar's SSE feed — the mechanism that
// lets a human reply reach the running session live, for streamInput()
// (docs/adr/0021 point 2), instead of the agent having to proactively poll
// for it. Runs until aborted via signal; onEntry is awaited serially so
// callers don't need their own queueing.
export async function streamHumanMessages(
  onEntry: (entry: TranscriptEntry) => void | Promise<void>,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch(`${base}/human-messages`, { signal });
  if (!res.ok || !res.body) throw new Error(`sidecar human-messages stream failed: ${res.status}`);

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return;
      buffer += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buffer.indexOf("\n\n")) !== -1) {
        const chunk = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
        const line = chunk.split("\n").find((l) => l.startsWith("data: "));
        if (!line) continue;
        const entry = JSON.parse(line.slice("data: ".length)) as TranscriptEntry;
        await onEntry(entry);
      }
    }
  } catch (err) {
    if (signal.aborted) return; // expected on shutdown, not a real failure
    throw err;
  } finally {
    reader.releaseLock();
  }
}
