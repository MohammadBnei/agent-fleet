# ADR-0050 — Playwright's browsers live on the shared PVC, not in the worker image

**Status:** Accepted
**Date:** 2026-08-16
**Amends:** `0044` (the browser install itself), `0048` §6 (one image per
session). **Related:** `0032` (the shared PVC as a no-rebuild channel).

## Context

`worker/Dockerfile` shipped a 5.13 GB image. One `RUN` accounted for 2.45 GB
of it:

| | size |
|---|---|
| browser builds (`/ms-playwright`, two Chromium pairs + ffmpeg) | **2.0 GB** |
| apt system libraries (`playwright install --with-deps`) | ~400 MB |
| npm packages (`playwright`, `@playwright/mcp`, `playwright-core`) | ~40 MB |

Every session carried all of it, on every node, re-pulled on every image
bump — including the large majority that never open a page.

The obvious fix was a second image: a slim `base` and a `worker-playwright`
variant, selected per repo through `repos.image`. It was built and measured
(1.87 GB / 5.13 GB) and **rejected on review**, for a reason that no size
number shows: it decides at pod-creation time whether a session can ever use a
browser. A session that turns out to need one has to be re-warmed onto a
different image, and `repos.image` is per *repo*, so the choice is coarser than
the need. Trading agility for image size is a bad trade for a fleet whose whole
premise is a session doing whatever the work turns out to require.

The other candidate was a separate browser pod, like the deleted e2e sandbox.
Rejected: `docs/adr/0044` is the record of browser-in-another-pod being dead
for the fleet's entire history behind three stacked failures, every one of them
a property of the cross-pod hop, and `docs/adr/0048` §6 made MCP local-only
specifically so nothing crosses a pod boundary again. It also reintroduces the
working-tree problem — the session PVC is `ReadWriteOnce`, so a second pod
cannot mount the tree, and reaching the app under test means rebuilding
`expose(port)` + Service + IngressRoute.

## Decision

**Split the layer by what each part actually is, not by which repo wants it.**

- The **system libraries stay in the image**. They are apt packages installed
  as root at build time; no mount can provide them. `playwright install-deps
  chromium` installs exactly these and downloads no browser.
- The **browser builds move to the shared PVC**, mounted read-only at
  `/ms-playwright`, with `PLAYWRIGHT_BROWSERS_PATH` pointing at it. They are
  large, version-pinned, and never written at runtime — the same shape
  `/repo-cache` already has.

One image, **2.41 GB**. Every session can browse, always. No variant, no second
pod, no MCP call leaving the pod.

Read-only is a real boundary, not decoration: one session cannot corrupt the
browser build every other session launches, which is the shared-writable-volume
pattern `docs/DECISIONS.md` forbids.

### The cache is filled by a Job on the worker image

`EnsureBrowserCache` (`provisioner/internal/k8s/browsercache.go`) creates a
fixed-name Job at provisioner startup, running **the worker image** against the
shared PVC. Both of `docs/adr/0044`'s installs run there — `playwright install
chromium` *and* `@playwright/mcp install-browser chrome-for-testing` — because
those two packages bundle different `playwright-core` versions and resolve
different build numbers, and one alone leaves the MCP server unable to launch.
That finding is unchanged; only its location moved.

The worker image, specifically, because the versions that decide which builds
are correct live in *its* `node_modules`. Resolving them from the provisioner
(a Go/debian image with no bun) is how the two drift apart.

Idempotent by construction — Playwright skips a build already present — so it
is safe to fire on every start with no marker to maintain, and an image bump
that changes a build number repairs the cache on the next start. The Job is
recreated only when it references a different image.

Best-effort, never fatal: a fleet whose cache is still filling cannot browse
yet, which is strictly better than a provisioner that refuses to start over it.

## Consequences

- **5.13 GB → 2.41 GB**, and the 2 GB lives once on the shared PVC instead of
  in a layer on every node for every image version.
- Browser availability stops being a per-repo decision. `repos.image` is still
  the per-repo toolchain knob; it is no longer the browser knob.
- **A new bootstrap window**: between a fresh cluster starting and the cache
  Job finishing (~2 GB download), browser calls fail. They fail *loudly* —
  Playwright names the exact missing path ("Executable doesn't exist at
  /ms-playwright/chromium_headless_shell-1234/…") — which is why the MCP server
  is still registered unconditionally rather than hidden when the directory is
  empty. Silently absent tools are `docs/adr/0044`'s failure mode; a clear
  error is not.
- The shared PVC gains a ~2 GB resident directory. It is 30 Gi.
- Verified by driving a real `browser_navigate` through `playwright-mcp`
  against a read-only mount, in the actual built image, plus a negative control
  with the mount absent. `docs/adr/0044`'s lesson is that nothing cheaper
  counts: the port being bound, the server answering, and 24 tools registering
  all passed while the browser was missing.

## Alternatives considered

- **Two image variants (`base` / `playwright`), chosen per repo.** Built and
  measured before being rejected. Smaller floor (1.87 GB) but it makes browsing
  a provisioning-time decision — see Context.
- **A separate browser pod.** Reopens `docs/adr/0044` and `0048` §6 — see
  Context.
- **Deduplicate the two Chromium builds** by pinning `playwright` and
  `@playwright/mcp` to versions sharing one `playwright-core`: ~5.13 GB →
  ~4.1 GB, no agility change, and it re-fights the version fight
  `docs/adr/0044` already lost.
- **Download browsers into the per-session PVC on first use.** Perfect
  agility, but every session pays a 2 GB download, and the session PVC is
  `local-path` on a node — the opposite of sharing.
