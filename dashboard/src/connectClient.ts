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
// app backgrounded on mobile), the in-flight subscription is aborted, and
// becoming visible again resumes from the last cursor position. This
// prevents "failed to fetch" errors when mobile browsers suspend network
// connections for backgrounded tabs.
//
// Each attempt needs its OWN AbortController, chained to the outer one:
// aborting on hide has to leave something left to abort with on unmount,
// and the outer controller is a one-shot. Pausing without aborting (what
// this used to do) only gated the *next* loop iteration — the open stream
// stayed open, the phone suspended its socket underneath it, and the throw
// arrived anyway, followed by a 1s retry loop hammering a network that
// wasn't there.
// What opens one stream attempt. Defaulted to the real client below; the
// only reason it is a parameter at all is that this loop's actual job —
// pause on hide, drop the connection, resume from the cursor — is worth
// testing without a server or a browser, and mocking the module instead
// turned out to depend on bun-version-specific `mock.module` timing (it
// silently didn't apply on CI's bun, so the "test" exercised the real
// transport against a relative URL). Production callers pass three args.
type OpenStream = (
  req: { sessionId: string; sinceSeq: bigint },
  opts: { signal: AbortSignal },
) => AsyncIterable<TranscriptEntry>;

export function subscribeTranscript(
  sessionId: string,
  since: bigint,
  onEntry: (entry: TranscriptEntry) => void,
  openStream: OpenStream = (req, opts) => client.streamTranscript(req, opts),
): () => void {
  const controller = new AbortController();
  let attempt: AbortController | null = null;
  let cursor = since;

  async function streamLoop() {
    while (!controller.signal.aborted) {
      // Wait while hidden (mobile background, tab switch) — no request is
      // in flight here, so nothing to fail.
      if (document.hidden) {
        await new Promise((resolve) => setTimeout(resolve, 100));
        continue;
      }

      attempt = new AbortController();
      const abortAttempt = () => attempt?.abort();
      controller.signal.addEventListener("abort", abortAttempt, { once: true });
      try {
        for await (const entry of openStream({ sessionId, sinceSeq: cursor }, { signal: attempt.signal })) {
          cursor = entry.seq + 1n;
          onEntry(entry);
        }
      } catch {
        // stream ended or dropped — fall through to the resubscribe delay
        // below unless we were deliberately aborted (cleanup, not a retry).
        // Distinguishing abort from network error helps avoid noise in logs.
        if (controller.signal.aborted) return;
      } finally {
        controller.signal.removeEventListener("abort", abortAttempt);
        attempt = null;
      }
      if (controller.signal.aborted) return;
      await new Promise((resolve) => setTimeout(resolve, RECONNECT_DELAY_MS));
    }
  }

  function handleVisibilityChange() {
    // Drop the connection now rather than letting the OS kill it and
    // surface as an error. `cursor` survives, so the loop resubscribes
    // without loss once the page is visible again.
    if (document.hidden) attempt?.abort();
  }

  // Listen for page visibility changes (tab switch, app backgrounding).
  // No initial-state seeding needed — streamLoop reads document.hidden
  // itself on every iteration.
  document.addEventListener("visibilitychange", handleVisibilityChange);

  void streamLoop();

  return () => {
    document.removeEventListener("visibilitychange", handleVisibilityChange);
    controller.abort();
  };
}
