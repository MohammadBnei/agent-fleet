// Same-origin API client — fleet-core serves this SPA's own build output,
// so no base URL is needed. Write calls set the required custom header
// (see fleet-core/internal/dashboard/csrf.go) so only this same-origin JS,
// never a third-party page holding a cached BasicAuth credential, can
// trigger them.
const DASHBOARD_HEADER = "X-Fleet-Dashboard";

export interface Task {
  id: string;
  repo: string;
  description: string;
  status: string;
  threadId?: string;
  prUrl?: string;
}

export interface TranscriptEntry {
  seq: number;
  from: string;
  text: string;
  type: string;
}

async function json<T>(res: Response): Promise<T> {
  if (!res.ok) {
    throw new Error(`${res.status} ${res.statusText}: ${await res.text()}`);
  }
  return res.json() as Promise<T>;
}

export function listTasks(): Promise<Task[]> {
  return fetch("/api/tasks").then(json<Task[]>);
}

export function getTask(id: string): Promise<Task> {
  return fetch(`/api/tasks/${id}`).then(json<Task>);
}

export function getTranscript(
  id: string,
  since = 0,
): Promise<{ entries: TranscriptEntry[]; nextSeq: number }> {
  return fetch(`/api/tasks/${id}/transcript?since=${since}`).then(
    json<{ entries: TranscriptEntry[]; nextSeq: number }>,
  );
}

export function getE2eStatus(
  id: string,
): Promise<{ status: string; previewUrl: string }> {
  return fetch(`/api/tasks/${id}/e2e-status`).then(
    json<{ status: string; previewUrl: string }>,
  );
}

export function subscribeToTranscript(
  id: string,
  since: number,
  onEntry: (entry: TranscriptEntry) => void,
): () => void {
  const source = new EventSource(`/api/tasks/${id}/events?since=${since}`);
  source.onmessage = (event) => {
    onEntry(JSON.parse(event.data) as TranscriptEntry);
  };
  return () => source.close();
}

function writeAction(path: string, body?: unknown): Promise<unknown> {
  return fetch(path, {
    method: "POST",
    headers: {
      [DASHBOARD_HEADER]: "1",
      ...(body ? { "Content-Type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  }).then(json);
}

export function approveTask(id: string): Promise<unknown> {
  return writeAction(`/api/tasks/${id}/approve`);
}

export function stopTask(id: string, reason?: string): Promise<unknown> {
  return writeAction(`/api/tasks/${id}/stop`, reason ? { reason } : undefined);
}

export function killE2e(id: string): Promise<unknown> {
  return writeAction(`/api/tasks/${id}/e2e/kill`);
}
