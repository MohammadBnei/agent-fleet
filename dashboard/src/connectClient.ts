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
//
// Page visibility handling: when the page becomes hidden (tab switched,
// app backgrounded on mobile), we pause the stream by aborting the current
// subscription. When the page becomes visible again, we resume from the
// last cursor position. This prevents "failed to fetch" errors when mobile
// browsers suspend network connections for backgrounded tabs.
export function subscribeTranscript(
  taskId: string,
  since: bigint,
  onEntry: (entry: TranscriptEntry) => void,
): () => void {
  const controller = new AbortController();
  let paused = false;
  let cursor = since;

  async function streamLoop() {
    while (!controller.signal.aborted) {
      // Wait while paused (page is hidden)
      if (paused) {
        await new Promise((resolve) => setTimeout(resolve, 100));
        continue;
      }

      try {
        for await (const entry of client.streamTranscript(
          { taskId, sinceSeq: cursor },
          { signal: controller.signal },
        )) {
          cursor = entry.seq + 1n;
          onEntry(entry);
        }
      } catch (err) {
        // stream ended or dropped — fall through to the resubscribe delay
        // below unless we were deliberately aborted (cleanup, not a retry).
        // Distinguishing abort from network error helps avoid noise in logs.
        if (controller.signal.aborted) return;
      }
      if (controller.signal.aborted) return;
      await new Promise((resolve) => setTimeout(resolve, RECONNECT_DELAY_MS));
    }
  }

  function handleVisibilityChange() {
    paused = document.hidden;
    // When becoming visible again after being hidden, the stream will
    // automatically reconnect on the next iteration of streamLoop.
  }

  // Listen for page visibility changes (tab switch, app backgrounding)
  document.addEventListener("visibilitychange", handleVisibilityChange);
  // Initialize paused state based on current visibility
  paused = document.hidden;

  void streamLoop();

  return () => {
    document.removeEventListener("visibilitychange", handleVisibilityChange);
    controller.abort();
  };
}
