import {
  Code,
  ConnectError,
  createClient,
  type Interceptor,
} from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AgentService } from "./gen/agentfleet/v1/agent_pb";
import { DashboardService } from "./gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

// Required on every call, unary and streaming alike (server-enforced too,
// see core/internal/dashboard/interceptor.go) — BasicAuth credentials
// are auto-attached by browsers to same-origin requests regardless of
// which page triggered them, so a required custom header (unsettable by a
// plain form/img, and blocked by CORS for any cross-origin fetch that
// tries) is what actually stops a third-party page from forging a call.
const DASHBOARD_HEADER = "X-Fleet-Dashboard";

const csrfInterceptor: Interceptor = (next) => async (req) => {
  req.header.set(DASHBOARD_HEADER, "1");
  return next(req);
};

// core federates the console's login to authentik and refuses an
// unauthenticated RPC with `unauthenticated` (infra-bootstrap ADR-0041).
//
// Without this, the session's 12h expiry is invisible and permanent: every
// caller here either swallows the error or retries, and subscribeTranscript's
// bare `catch {}` reconnects once a second, forever, against a gate that will
// never open. The console keeps rendering from cache and simply stops updating
// — an outage, not a login prompt. It would hit daily, mid-session.
//
// A redirect here, not a re-request: the server deliberately answers a fetch()
// with 401 rather than a 302, because a 302 is followed transparently and would
// hand authentik's HTML to a JSON parser — the shape that took ArgoCD's login
// down (infra-bootstrap #189).
const authInterceptor: Interceptor = (next) => async (req) => {
  try {
    return await next(req);
  } catch (err) {
    if (err instanceof ConnectError && err.code === Code.Unauthenticated) {
      const returnTo = encodeURIComponent(
        window.location.pathname + window.location.search,
      );
      window.location.assign(`/auth/login?return_to=${returnTo}`);
    }
    throw err;
  }
};

const transport = createConnectTransport({
  baseUrl: "/",
  interceptors: [csrfInterceptor, authInterceptor],
});

export const client = createClient(DashboardService, transport);

// The decision deck's API (docs/adr/0082 §5) is on AgentService, served on the
// same origin behind the same CSRF header. Without the login redirect: with
// FLEET_AUTH_DISABLED=1 every AgentService call is Unauthenticated, and that
// must not bounce a local console to a login route that does not exist. An
// expired session still redirects through `client`'s next poll.
export const agent = createClient(
  AgentService,
  createConnectTransport({ baseUrl: "/", interceptors: [csrfInterceptor] }),
);

const RECONNECT_DELAY_MS = 1000;

// A server stream in a resubscribe-on-drop retry loop.
// EventSource (what this replaced) auto-reconnects on a dropped
// connection; a bare `for await` over a Connect stream does not. This
// whole system's pull/cursor design exists specifically so a reconnect
// can resume without loss (docs/adr/0013) — losing that here would
// silently regress a property the architecture already paid for. Where to
// resume from is the caller's: `open` reads its own cursor on every attempt.
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
//
// `isFatal` names an error no retry can fix: the loop stops for good on it.
export function resubscribe<T>(
  open: (signal: AbortSignal) => AsyncIterable<T>,
  onMsg: (msg: T) => void,
  isFatal?: (err: unknown) => boolean,
): () => void {
  const controller = new AbortController();
  let attempt: AbortController | null = null;

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
        for await (const msg of open(attempt.signal)) onMsg(msg);
      } catch (err) {
        // stream ended or dropped — fall through to the resubscribe delay
        // below unless we were deliberately aborted (cleanup, not a retry).
        // Distinguishing abort from network error helps avoid noise in logs.
        if (controller.signal.aborted) return;
        if (isFatal?.(err)) return;
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
    // surface as an error. The caller's cursor survives, so the loop
    // resubscribes without loss once the page is visible again.
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

// client.streamTranscript, resumed from the entry after the last one seen.
export function subscribeTranscript(
  sessionId: string,
  since: bigint,
  onEntry: (entry: TranscriptEntry) => void,
  openStream: OpenStream = (req, opts) => client.streamTranscript(req, opts),
): () => void {
  let cursor = since;
  return resubscribe(
    (signal) => openStream({ sessionId, sinceSeq: cursor }, { signal }),
    (entry) => {
      cursor = entry.seq + 1n;
      onEntry(entry);
    },
  );
}

type OpenWatch = (
  req: { sinceVersion: bigint },
  opts: { signal: AbortSignal },
) => AsyncIterable<unknown>;

// The decisions store's change feed (docs/adr/0082 §5), as a bare "something
// changed": every message, the baseline included, has the caller re-list. So
// there is nothing to resume from and every attempt starts at the current
// version; the server ends each stream after 90s and the loop reopens it.
//
// Unauthenticated (FLEET_AUTH_DISABLED=1: AgentService refuses every call) and
// PermissionDenied never heal on a retry: the watch stops for good and says so
// once, and the caller falls back to what it had before the store.
export function watchDecisions(
  onChange: () => void,
  onUnavailable: () => void,
  open: OpenWatch = (req, opts) => agent.watchDecisions(req, opts),
): () => void {
  return resubscribe(
    (signal) => open({ sinceVersion: 0n }, { signal }),
    onChange,
    (err) => {
      const fatal =
        err instanceof ConnectError &&
        (err.code === Code.Unauthenticated ||
          err.code === Code.PermissionDenied);
      if (fatal) onUnavailable();
      return fatal;
    },
  );
}
