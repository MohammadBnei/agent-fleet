import { useEffect, useState } from "react";

export type Identity = { authenticated: boolean; email?: string; groups?: string[] };

// Who the console thinks you are, for its own chrome.
//
// The gap this fills is small but real: with basic-admin-auth removed,
// fleet.bnei.dev is gated by authentik alone, and the sign-in redirect is fast
// enough that nothing on screen ever said an identity was involved. The
// console looked exactly as it did when one shared password let anyone in.
//
// Plain fetch against /auth/me rather than a DashboardService RPC: one line of
// chrome does not justify a proto change, codegen in two languages and a buf
// breaking-change check.
//
// Deliberately NOT the generated client. That transport turns a 401 into a
// redirect to /auth/login, so asking "am I signed in" through it would bounce a
// signed-out visitor to authentik before the page rendered. /auth/me answers
// 200 with authenticated:false instead, and this hook stays silent about it.
//
// 404 is the FLEET_AUTH_DISABLED=1 local stack, where the route is not
// registered at all. Also silent, and correct: there is no identity to show.
export function useIdentity(): Identity | null {
  const [identity, setIdentity] = useState<Identity | null>(null);

  useEffect(() => {
    let cancelled = false;
    void fetch("/auth/me", { credentials: "same-origin" })
      .then((res) => (res.ok ? (res.json() as Promise<Identity>) : null))
      .then((data) => {
        if (!cancelled && data?.authenticated) setIdentity(data);
      })
      .catch(() => {
        // A console that cannot say who you are is not a console that should
        // stop working. Nothing here gates anything — the real check is
        // server-side on every request.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return identity;
}
