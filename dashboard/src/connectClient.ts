import { createConnectTransport } from "@connectrpc/connect-web";
import { createClient, type Interceptor } from "@connectrpc/connect";
import { DashboardService } from "./gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// Required on every call, unary and streaming alike (server-enforced too,
// see fleet-core/internal/dashboard/interceptor.go) — BasicAuth credentials
// are auto-attached by browsers to same-origin requests regardless of
// which page triggered them, so a required custom header (unsettable by a
// plain form/img, and blocked by CORS for any cross-origin fetch that
// tries) is what actually stops a third-party page from forging a call.
const DASHBOARD_HEADER = "X-Fleet-Dashboard";

const csrfInterceptor: Interceptor = (next) => async (req) => {
  req.header.set(DASHBOARD_HEADER, "1");
  return next(req);
};

const transport = createConnectTransport({
  baseUrl: "/",
  interceptors: [csrfInterceptor],
});

export const client = createClient(DashboardService, transport);

const RECONNECT_DELAY_MS = 1000;

// Wraps client.streamTranscript in a resubscribe-on-drop retry loop.
// EventSource (what this replaced) auto-reconnects on a dropped
// connection; a bare `for await` over a Connect stream does not. This
// whole system's pull/cursor design exists specifically so a reconnect
// can resume without loss (docs/adr/0013) — losing that here would
// silently regress a property the architecture already paid for.
export function subscribeTranscript(
  taskId: string,
  since: bigint,
  onEntry: (entry: TranscriptEntry) => void,
): () => void {
  const controller = new AbortController();

  void (async () => {
    let cursor = since;
    while (!controller.signal.aborted) {
      try {
        for await (const entry of client.streamTranscript(
          { taskId, sinceSeq: cursor },
          { signal: controller.signal },
        )) {
          cursor = entry.seq + 1n;
          onEntry(entry);
        }
      } catch {
        // stream ended or dropped — fall through to the resubscribe delay
        // below unless we were deliberately aborted (cleanup, not a retry).
      }
      if (controller.signal.aborted) return;
      await new Promise((resolve) => setTimeout(resolve, RECONNECT_DELAY_MS));
    }
  })();

  return () => controller.abort();
}
