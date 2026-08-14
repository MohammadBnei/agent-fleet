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
import { log } from "./log.js";

// A function, not a module-level const: Bun's module cache is
// process-global, and worker/src/index.test.ts already documents (and
// depends on) the fact that this module gets imported for real by more
// than one test file in the same process. A const computed once at first
// import would freeze whichever SIDECAR_API_ADDR happened to be set at
// that moment — this file's own tests set it per-test, after the module
// may already be cached, so it has to be read live on every call.
function base(): string {
  return `http://${process.env.SIDECAR_API_ADDR ?? "localhost:9091"}`;
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`${base()}${path}`, {
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

// leaseId lets core reject this write if the pod has already been replaced
// (docs/adr/0041) — tasks.session_id is what the next resume reads, so a
// dead pod setting it would resume the wrong conversation.
export async function saveSessionId(sessionId: string, model: string): Promise<void> {
  await postJSON("/session-id", { sessionId, model, leaseId: process.env.LEASE_ID ?? "" });
}

// getSession reads the session row fresh at startup.
//
// It survives docs/adr/0048's cull of this API for exactly one reason:
// permission_mode must be RESTORED on a warm. Without it, resuming a session
// a human put into acceptEdits/plan/bypassPermissions silently reverts it to
// "default" — and the only way to notice is to watch it start asking for
// permission again.
//
// `description` comes back too but is a label, not an instruction: the
// session's actual instruction is its first transcript entry. `guidance` and
// `baseBranch` are gone with the columns.
export async function getSession(): Promise<{
  description: string;
  permissionMode?: string;
  model?: string;
}> {
  const res = await fetch(`${base()}/task`);
  if (!res.ok) throw new Error(`sidecar /task failed: ${res.status} ${await res.text()}`);
  return res.json();
}

export async function savePermissionMode(mode: string): Promise<void> {
  await postJSON("/permission-mode", { mode });
}

export async function stillHoldsLease(leaseId: string): Promise<boolean> {
  const res = await fetch(`${base()}/still-holds-lease?leaseId=${encodeURIComponent(leaseId)}`);
  if (!res.ok) throw new Error(`sidecar still-holds-lease failed: ${res.status}`);
  const body = (await res.json()) as { holds: boolean };
  return Boolean(body.holds);
}

// pushMessage lets the wrapper post directly into the transcript — the
// round-cap checkpoint text, the agent's raw assistant narration (today
// relayed to Discord by the now-deleted discord.ts; both now route through
// core's own relay loop, uniformly with the agent's own send_message posts),
// and canUseTool's own permission_request entries, which need the returned
// seq back to correlate the eventual permission_response reply.
// `replyTo` correlates this entry to an earlier one — used when the wrapper
// records a permission it resolved itself, so the entry points at the
// request it answers exactly like the dashboard's own response entries do.
export async function pushMessage(from: string, text: string, type?: string, replyTo?: number): Promise<number> {
  const res = await postJSON<{ seq: number }>("/message", { from, text, type, replyTo });
  return res.seq;
}

export type TranscriptEntry = { seq: number; from: string; text: string; type?: string; replyTo?: number };

const RECONNECT_DELAY_MS = 2000;

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise<void>((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}

// streamHumanMessages consumes the sidecar's SSE feed — the mechanism that
// lets a human reply reach the running session live, for streamInput()
// (docs/adr/0021 point 2), instead of the agent having to proactively poll
// for it. Runs until aborted via signal; onEntry is awaited serially so
// callers don't need their own queueing.
//
// Reconnects automatically instead of ever giving up — confirmed live in
// prod: this feed can go quiet at the wire level for as long as there's
// simply nothing new to deliver, and once something upstream eventually
// closes that idle connection (a redeploy, an idle-connection reaper,
// anything), the old version of this function just returned or threw once
// and was never called again — permanently ending the task's ability to
// receive another human message (a permission decision, Discuss, even Abort) for the
// rest of its life, with nothing but one easy-to-miss log line as
// evidence. nextSeq tracks how far we've actually delivered so a
// reconnect resumes exactly there via the sidecar's sinceSeq query param
// — no replayed or skipped messages.
//
// sinceSeq is the first *unread* seq, not the last read one: core's
// ReadSince is `WHERE seq >= $1` (internal/transcript/postgres.go) and
// hands back `nextSeq = lastEntry.Seq + 1`, the same convention the
// dispatch path (LatestSeq -> RESUME_FROM_SEQ) and the dashboard's own
// cursor use. Storing entry.seq here instead re-delivered the newest
// human message on every single reconnect, and the SDK takes a repeated
// streamed input as a fresh user turn — so the agent visibly answered the
// same question twice, minutes apart, with nothing in the transcript
// explaining why.
//
// startSeq seeds the initial cursor (default 0, a brand-new task's
// natural starting point) — a resumed/warmed pod must pass its own
// dispatch-time RESUME_FROM_SEQ instead, or this replays every
// pre-existing human directive from task history, including a stale
// Stop's "abort" entry, which self-aborts the new session immediately.
export async function streamHumanMessages(
  onEntry: (entry: TranscriptEntry) => void | Promise<void>,
  signal: AbortSignal,
  startSeq = 0,
): Promise<void> {
  let nextSeq = startSeq;
  while (!signal.aborted) {
    try {
      await streamOnce(
        nextSeq,
        async (entry) => {
          await onEntry(entry);
          nextSeq = entry.seq + 1;
        },
        signal,
      );
      if (signal.aborted) return;
      log("warn", "human-messages stream ended, reconnecting", { nextSeq });
    } catch (err) {
      if (signal.aborted) return;
      log("warn", "human-messages stream failed, reconnecting", { nextSeq, error: String(err) });
    }
    await sleep(RECONNECT_DELAY_MS, signal);
  }
}

// Runs one SSE connection until the server ends it (returns) or something
// goes wrong (throws) — the caller loop above is what actually reconnects.
// Comment-only lines (the sidecar's keep-alive pings) have no "data: "
// line and are silently skipped, same as before.
async function streamOnce(
  sinceSeq: number,
  onEntry: (entry: TranscriptEntry) => void | Promise<void>,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch(`${base()}/human-messages?sinceSeq=${sinceSeq}`, { signal });
  if (!res.ok || !res.body) throw new Error(`sidecar human-messages stream failed: ${res.status}`);

  const reader = res.body.getReader();
  // Belt-and-suspenders: fetch()'s own `signal` should cancel an in-flight
  // reader.read() once aborted, but that's not reliably immediate across
  // every runtime/timing — an abort fired synchronously from inside
  // onEntry (exactly what every caller does to end a session cleanly) can
  // leave a *subsequent* reader.read() call hanging indefinitely, since
  // nothing forces that read to settle. Explicitly cancelling the reader
  // guarantees any pending/future read() rejects promptly regardless of
  // whether the fetch-level signal wiring caught it in time.
  const onAbort = () => void reader.cancel().catch(() => {});
  signal.addEventListener("abort", onAbort, { once: true });
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
        // Don't wait on another read() at all once aborted — that's the
        // exact hang this is guarding against, not just a fallback for it.
        if (signal.aborted) return;
      }
    }
  } catch (err) {
    if (signal.aborted) return; // expected on shutdown, not a real failure
    throw err;
  } finally {
    signal.removeEventListener("abort", onAbort);
    reader.releaseLock();
  }
}
