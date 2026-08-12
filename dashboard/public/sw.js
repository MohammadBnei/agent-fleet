// Service worker for the herd console.
//
// This app is a live operator console, which decides everything below: it exists
// to make an installable home-screen app out of the dashboard, NOT to work
// offline in any meaningful sense. Caching fleet state would be actively harmful —
// a human acting on a cached task list would be answering a decision that may
// already be resolved. So:
//
//   * only same-origin GETs are touched. Every ConnectRPC call is a POST to
//     /agentfleet.v1.*, so they bypass this by construction; the explicit path
//     check below is belt-and-braces in case a future RPC ever uses GET.
//   * navigations are network-FIRST, cache only as an offline fallback. A
//     cache-first shell is the classic PWA footgun: after a redeploy the operator
//     keeps running last week's UI against this week's API.
//   * content-hashed assets under /assets/ are cache-first, since their URL
//     changes whenever their content does.
//
// There is no build-time precache list, and deliberately no workbox/
// vite-plugin-pwa dependency: caching on first use gets the same result for a
// single-page app whose assets are already content-hashed by vite.

// Bump to evict everything from older workers.
const CACHE = "herd-v1";

// Populated on first navigation; served when the network is gone.
const SHELL = "/";

self.addEventListener("install", (event) => {
  // Warm the shell so the very first offline load has something to render.
  event.waitUntil(
    caches
      .open(CACHE)
      .then((cache) => cache.add(new Request(SHELL, { cache: "reload" })))
      .catch(() => {})
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      // Take over open tabs immediately: a console left open for hours must not
      // keep an old worker (and old shell) alive after a deploy.
      .then(() => self.clients.claim()),
  );
});

function cacheable(response) {
  // Never store a redirect, an error, or a cross-origin opaque response.
  return response && response.ok && response.type === "basic";
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  // Fleet state, never cached — see the header comment.
  if (url.pathname.startsWith("/agentfleet.v1.")) return;
  if (url.pathname === "/healthz") return;
  // Server-sent/streamed responses must not be buffered through a cache.
  if (req.headers.get("accept")?.includes("text/event-stream")) return;

  // Immutable, content-hashed build output: cache-first is safe and is what
  // makes a warm load instant.
  if (url.pathname.startsWith("/assets/")) {
    event.respondWith(
      caches.match(req).then(
        (hit) =>
          hit ??
          fetch(req).then((res) => {
            if (cacheable(res)) {
              const copy = res.clone();
              caches.open(CACHE).then((c) => c.put(req, copy));
            }
            return res;
          }),
      ),
    );
    return;
  }

  // The app shell. Network-first so a redeploy is picked up on the next load;
  // the cache is only the offline answer.
  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req)
        .then((res) => {
          if (cacheable(res)) {
            const copy = res.clone();
            caches.open(CACHE).then((c) => c.put(SHELL, copy));
          }
          return res;
        })
        .catch(() => caches.match(SHELL).then((hit) => hit ?? Response.error())),
    );
    return;
  }

  // Everything else static (icons, favicon, manifest): try the network, fall
  // back to whatever we have.
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (cacheable(res)) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return res;
      })
      .catch(() => caches.match(req).then((hit) => hit ?? Response.error())),
  );
});
