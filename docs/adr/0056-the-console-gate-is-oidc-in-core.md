# ADR-0056 — The console's gate is OIDC inside core, not forwardAuth at the ingress

- **Status:** Accepted
- **Date:** 2026-08-19
- **Related:** [#200](https://github.com/MohammadBnei/agent-fleet/issues/200)
  (a worker pod can call `DashboardService`), [#209](https://github.com/MohammadBnei/agent-fleet/issues/209)
  (the issue this rejects the approach of), infra-bootstrap ADR-0041 and its
  same-day amendment, [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md)
  (the other half of #200)

## Context

`fleet.bnei.dev` was gated by one shared `apr1`/MD5 basic-auth credential whose
hash is in `k8s-cluster`'s git history — the same credential on pgweb,
Alertmanager and Proxmox. Behind it: agents holding repo write tokens and,
through thot, cluster RBAC. It is also the only `*.bnei.dev` host still grey at
Cloudflare, so none of the perimeter work (WAF, geo rules, origin lock) applies
to it.

#209 and infra-bootstrap ADR-0039 both proposed the same fix: a Traefik
forwardAuth middleware in front of Traefik, using an authentik proxy provider.
That is the right shape for a service that cannot federate, and `core` could
not.

The reason it is wrong **here** is not about what core can do. It is that
**a middleware gates the ingress and has no opinion about a caller that never
reaches the ingress.** #200 is exactly that caller: a worker pod POSTing to
`agent-fleet-core.agent-fleet.svc.cluster.local:8080` on the pod network,
behind nothing but the `X-Fleet-Dashboard` CSRF header. forwardAuth would have
left that untouched while looking solved — the worst combination available.

## Decision

1. **`core` is an OIDC relying party.** Authorization-code flow against
   authentik with `state`, `nonce` and PKCE; a signed session cookie; and a
   required `platform-admins` group claim. The check runs inside the process, so
   it applies regardless of the caller's network position — the property
   forwardAuth structurally cannot have.

2. **Discovery, never hardcoded endpoint paths.** authentik's `userinfo`
   endpoint is **not** underneath its per-application issuer, so anything that
   builds it by concatenation produces a doubled path that 404s with an HTML
   body. That took ArgoCD's login down on 2026-08-19 (infra-bootstrap #189), a
   day before this shipped.

3. **The issuer string is passed through unmodified.** OIDC compares it
   byte-for-byte against the discovery document, and authentik's ends in a
   trailing slash. See Consequences — this one was learned the expensive way.

4. **Fail closed.** Unset OIDC config makes core refuse to start, unlike every
   other optional secret it reads. The Infisical operator renders stale or empty
   Secrets often enough that "feature off when unset" would bring core up wide
   open and looking healthy. `FLEET_AUTH_DISABLED=1` is the explicit local-stack
   opt-out, so "no auth" is always something someone wrote down.

5. **The gate wraps the whole mux**, with an explicit exempt list, rather than
   the SPA handler alone. Gating one route makes every route added later public
   by default; this way a forgotten one fails closed. It must also sit above the
   SPA's `index.html` fallback, which answers any unknown path with a 200.

6. **The session cookie is `__Host-`prefixed.** Not decoration: the fleet
   publishes agent-authored dev servers at `<id>-e2e.bnei.dev`, which is
   **same-site** with the console. `SameSite` is no defence between same-site
   hosts, and without the prefix such a page could set a `Domain=bnei.dev`
   cookie shadowing the operator's session. The prefix makes the browser forbid
   `Domain` outright.

7. **The `X-Fleet-Dashboard` header stays**, and is the load-bearing
   anti-forgery control for that same reason. The session check stops an
   *unauthenticated* call; the header plus never allowing CORS stops a
   *cross-origin* one. Different jobs; keeping both is not redundancy.

8. **core does not read `X-authentik-*` headers**, then or later. It verifies
   its own ID token. A component reachable on the pod network can forge a
   header, so trusting one converts an authorization gap into an impersonation
   one — strictly worse. This is why the tier assignment is not a matter of
   taste for this host.

9. **`basic-admin-auth` was removed the same day**, once a real login was
   proven. It was kept only briefly, and it is worth recording that of its two
   reasons one *expired* (the deploy window where `k8s/core.yaml` could carry a
   middleware removal and a pre-OIDC image tag at once) and one was *overruled*
   (the break-glass). See Consequences.

## Consequences

**An authentik outage means no console at all.** authentik depends on Pigsty →
Patroni, so that chain now sits under every login, and core has no local admin
to fall back to — the asymmetry infra ADR-0039 Decision 6 named when it kept
local admins on Grafana and ArgoCD. Recovery is `FLEET_AUTH_DISABLED=1` plus a
redeploy, or `kubectl port-forward`. Accepted knowingly.

**Every path core serves on 8080 is now public unless the in-app gate refuses
it.** The console's IngressRoute matches on host with no path constraint. What
the gate exempts — `/healthz`, `/auth/*`, `/webhook/alertmanager` — is therefore
internet-reachable. The webhook keeps its own bearer token and refuses when it
is unset; the other two are inert. `/metrics` is deliberately **not** exempt.
**A new exempt path is now a new public endpoint.**

**Two failures shipped on the way in, both worth keeping.**

*The issuer was normalised.* `auth.New` passed it through
`strings.TrimSuffix(_, "/")` as tidying. OIDC compares the issuer byte-for-byte
and authentik's ends in a slash, so core crash-looped — and because this path
fails closed on purpose, a config-shaped bug here is an **outage**, not a
degraded login. The console was down ~15 minutes. Nothing could have caught it:
every auth test drove the signer, the gate and the cookie, and **no test
exercised real discovery**, so the whole path from config value to provider was
unobserved until it met authentik. `oidc_test.go` now closes that with an
httptest discovery stub shaped like authentik's.

*A variable shadowed a package.* This ADR's `auth` package import and
[ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md)'s local
`auth` variable landed in separate PRs. Both were green on their own branches,
neither branch contained the other's half, and there was **no textual
conflict** — so GitHub reported `MERGEABLE`, the squash was clean, and the build
broke only at the release. **Mergeable is not compiles.** The blast radius was
bounded by pipeline shape rather than by anyone noticing: `build-push` failed,
so `deploy` was skipped, so no image tag moved and the cluster kept running the
previous release.

**The console had no idea it was authenticated.** With basic-auth gone and the
authentik redirect being fast, the UI looked exactly as it had when one shared
password let anyone in — so there was no way to tell from the screen whether SSO
was on. Fixed with `/auth/me` and a `SIGNED IN` section. That endpoint answers a
signed-out caller `200 {"authenticated":false}`, never 401, because the SPA's
transport turns 401 into a login redirect and a 401 there would bounce a
signed-out visitor to authentik before anything rendered.

## Alternatives considered

- **forwardAuth for the console** (#209, infra ADR-0039 Decision 3 as
  originally written). Rejected: gates the ingress only, leaves #200 entirely
  open, and depends on a proxy provider plus an outpost binding landing first.
  **Kept for the e2e previews**, which route to a session's own dev-server pod
  with no fleet code in the request path, and whose hostnames are minted at
  runtime — so `forward_single` cannot name them and native OIDC has nowhere to
  live.
- **NetworkPolicy denying worker pods → core:8080.** Rejected as the whole
  answer: constrains network topology rather than establishing caller identity,
  gives no attribution, and a policy written but not enforced looks identical to
  one that works. Still worth having as a layer underneath.
- **Reading `X-authentik-*` headers for attribution.** Rejected — see Decision 8.
- **Keeping `basic-admin-auth` permanently.** Rejected by the operator in
  favour of single sign-on, with the availability cost stated rather than
  discovered.
