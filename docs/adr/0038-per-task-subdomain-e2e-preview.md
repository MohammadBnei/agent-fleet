# ADR-0038: Per-task subdomains for e2e previews (over path-based routing)

**Status:** Accepted
**Depends on:** `infra-bootstrap` ADR-0033 (Cloudflare DNS + the `le-dns` resolver)
**Supersedes (in part):** [ADR-0012](0012-e2e-provisioner-standalone-app.md)'s
path-based routing decision only. Everything else in ADR-0012 — the standalone
provisioner, its RBAC, the shared `basic-admin-auth` middleware — stands.

## Context

E2E preview pods were served under one static host with path-based routing:
`https://e2e.bnei.dev/<taskId>/app/`, with a Traefik `stripPrefix` middleware
removing `/<taskId>/app` before the request reached the target app's dev server.

**The target app was never told what its public base path was.** Nothing set a
`--base`/`basePath` in any seeded `start_cmd`, and nothing consumed the
`X-Forwarded-Prefix` header `stripPrefix` emits. So any app emitting
root-absolute URLs — `/assets/...`, `/api/...`, client-side router links —
404'd under the preview even when the pod was healthy and `appReady`. The app
built links against `/`, but `/` belonged to no task.

This is most target apps, not an edge case. `dream-analyst`'s `start_cmd` runs
`bunx vite dev`, and Vite emits `/assets/...` unconditionally.

ADR-0012 chose path routing for a sound reason at the time:

> Path-based routing under one static host (`e2e.bnei.dev/<taskId>/app/`,
> `/<taskId>/code/`), not per-task subdomains — no wildcard DNS exists anywhere
> in this cluster (ADR-0001 already rejected DNS-01/wildcard-cert issuance), so
> per-task subdomains aren't available without new ACME issuance per session.

That constraint was real. Let's Encrypt allows **50 new certificates per
registered domain per 7 days, shared globally across every subdomain**, so a
cert per preview session would have exhausted issuance for
`grafana`/`argocd`/`fleet`/`s3`.bnei.dev after roughly seven sessions a day.

**What changed:** `bnei.dev` moved to Cloudflare, which unlocked Traefik's
native DNS-01 resolver and therefore a **single wildcard certificate** for
`*.e2e.bnei.dev`. One cert, unlimited per-task subdomains, rate limit
irrelevant. See `infra-bootstrap` ADR-0033 for that decision, including why it
does not reopen the cert-manager ban.

## Decision

1. **One subdomain per task, app at the root.**
   `PreviewURLFor` returns `https://<shortId>.<E2E_HOST>/`. `shortID()` already
   produces a DNS-safe label (dashes stripped, truncated to 20 characters), so
   it is used directly — no new sanitisation.

2. **No `stripPrefix` on the app route.** This is the entire point: the app is
   served at `/`, so root-absolute URLs resolve and no app needs to know
   anything about its deployment.

3. **code-server keeps a path prefix**, at `<shortId>.<host>/code`, and keeps
   `stripPrefix`. It is proxy-aware and reads `X-Forwarded-Prefix` correctly,
   unlike an arbitrary target app. This keeps its URL derivable as
   `previewUrl + "code"`, so no proto change is needed.

4. **The wildcard is declared explicitly** on every task's `IngressRoute`
   (`tls.domains[0].main: "*.<host>"`, `certResolver: le-dns`). Every task
   asking for the *same* wildcard is what makes ACME order it once and reuse it
   forever. Left implicit, Traefik derives the concrete per-task hostname from
   the `Host()` rule and orders a certificate per session — the exact failure
   this design exists to avoid.

5. **Route priorities are explicit** (code 200, app 100). Rule length would
   already order them correctly, but leaving it implicit makes a target app
   that happens to own `/code` a silent coin flip.

6. **Auth is unchanged** — both routes keep the shared `basic-admin-auth`
   middleware, exactly as under ADR-0012.

## Consequences

- **Any target app works under preview without modification.** No `--base`
  flag, no `X-Forwarded-Prefix` handling, no per-repo `start_cmd` special-casing.
- **One certificate, ever**, for all preview sessions. The first session mints
  `*.e2e.bnei.dev`; every later one reuses it and issues nothing.
- **Preview URLs are no longer guessable from the task ID.** They are derived
  from `shortID()`, which is the truncated, dash-stripped task ID — recoverable,
  but not the raw UUID a human might paste.
- **A target app owning `/code` is shadowed** by the code-server route. Flagged
  with a `ponytail:` comment; the fix is giving code-server its own
  `<id>-code` subdomain, which is free now that the wildcard covers any label.
- **Sessions live across the deploy keep path routing** until torn down.
  `ignoreAlreadyExists` (`provisioner/internal/k8s/pod.go`) is create-if-absent
  with no reconcile-to-match — its own `ponytail:` comment already records this.
  Not worth server-side apply for a transient window.
- **This repo now depends on a resolver defined in `infra-bootstrap`.** A
  cluster without `le-dns` configured will fail to issue the preview
  certificate, and the failure surfaces as an ACME error in Traefik's logs
  rather than anywhere in this repo.
- `local/kind/` still cannot exercise any of this — kind has no Traefik CRDs,
  so `CreateIngressRoute`/`CreateMiddleware` already fail there. The
  fake-clientset tests in `internal/k8s` are the only automated coverage.

## Alternatives considered

- **Cookie-scoped routing on the single existing host.** A claim URL
  (`/_e2e/<taskId>`) setting a cookie via a Traefik `headers` middleware, with
  a `HeaderRegexp(`Cookie`, …)` router serving the app at `/`. Zero DNS and zero
  certificate changes, and it genuinely fixes the base-path problem. Rejected:
  it limits a browser to one active preview at a time, and a cookie-matching
  router is markedly more fragile than a hostname.
- **A fixed pool of preview hostnames** (`e2e1..e2eN.bnei.dev`), pre-issued via
  the existing TLS-ALPN-01 resolver — no DNS-01 and no new credential, at the
  cost of slot-allocation bookkeeping in the provisioner and a hard ceiling of N
  concurrent previews. Rejected once the zone move made wildcards free.
- **Teaching each app its base path** via `X-Forwarded-Prefix` or a `--base`
  flag in every `start_cmd`. Rejected: it pushes fleet-specific knowledge into
  every target repo, needs per-framework handling, and silently breaks whenever
  an app adds a new root-absolute URL.
- **A second subdomain for code-server** (`<id>-code.<host>`). Free under the
  wildcard and slightly cleaner, but it needs a new proto field to stay
  discoverable, whereas `/code` keeps the URL derivable from `previewUrl`.
  Recorded as the upgrade path if `/code` ever collides.
