# Security hardening — done and left

Caveman notes. Short words. Fleet view of a cluster-wide job.

Full record lives in infra-bootstrap: `docs/adr/0038`, `0039`, `0040`, and
`docs/bootstrap-test-notes.md`. This file says what changed, what it means for
this repo, and what still bites.

Started 2026-08-18. One sentence why: cluster was open window.

---

## 1. Wall outside — DONE

Was: DNS pointed straight at home IP. Anyone could reach Traefik. No filter.

Now: Cloudflare sits in front. Apex and `*.bnei.dev` proxied.

| Thing | State |
|---|---|
| Cloudflare proxy | on |
| WAF geo rules | on |
| TLS mode | Full (strict), both zones |
| TLS floor | 1.2 |
| Origin lock | Traefik `ipAllowList`: Cloudflare ranges + LAN + pod CIDR |
| Rate limit + security headers | baked into `common-app-chart` baseline, every app |
| Access logs | JSON, into Loki |
| HTTP → HTTPS | redirect on |

Cert engine had to move first. TLS-ALPN-01 dies behind a proxy — edge
terminates 443, challenge never reaches Traefik. Fails silent, up to 90 days
later. So `le` moved to DNS-01 over Cloudflare. Only then flip the cloud.
Order matters. Wrong order = no error, then dead certs in three months.

### Fleet is the hole

`fleet.bnei.dev` stays **grey**. DNS-only. No proxy.

Reason: ConnectRPC streaming. Cloudflare free plan sends HTTP 524 when origin
says nothing for 100s. Long stream trips it. Dashboard breaks.

Cost of grey:
- origin IP public
- no WAF
- no geo rule
- origin lock off (`k8s/core.yaml` — lock allowlists Cloudflare peers; on a
  grey host peer is the real client, so lock would 403 everyone)
- rate limit falls back to peer address, since `CF-Connecting-IP` never set

So every wall above protects every host **except this one**. Fleet is the
front door of the agency and the least covered.

Fix: heartbeat frame under 100s in streaming handlers. Then go orange. Open.

---

## 2. Inside the cluster — DONE

| Thing | Was | Now |
|---|---|---|
| Secrets in etcd | plaintext, all 3 CP nodes, every snapshot | `secretbox` encrypted |
| API audit log | none | on, tailed by Alloy into Loki |
| Node-to-node traffic | cleartext | Cilium WireGuard, 4 peers |

etcd encryption is one-way. Once Secrets rewritten, dropping the flag makes
them unreadable. Back up etcd before touching it. Do not "just revert".

---

## 3. Identity — PART DONE

authentik runs. `authentik.bnei.dev`. Postgres on Pigsty, not a second
in-cluster DB. Version 2026.8.0.

All config is **blueprints**. Declarative. Secrets in git as templates,
values from Infisical. Nothing clicked in the UI. Clicked config cannot be
reviewed and does not survive a rebuild.

| Tier | Apps | State |
|---|---|---|
| Native OIDC | Grafana | done, login works |
| Native OIDC | ArgoCD | done 2026-08-19 |
| forwardAuth | fleet, previews, Alertmanager, pgweb, Proxmox | **open** |
| Passkeys | Proxmox, ArgoCD, Infisical, Alertmanager | open |

Local admin stays on for Grafana and ArgoCD. On purpose. A LAN break-glass
route skips a Traefik middleware; it cannot skip an app's own OIDC redirect.
ArgoCD also deploys authentik. Drop local admin, lock self out.

---

## 4. What is left, fleet first

### 4.1 forwardAuth for fleet — infra #183, fleet #209

Today `fleet.bnei.dev` is gated by **one shared basic-auth password**. `apr1`
/ MD5. Same one on pgweb, Alertmanager, Proxmox. Hash is in `k8s-cluster` git
history.

Behind that door: agents with repo write access and, through thot, cluster
RBAC. Weakest lock on the biggest room.

Plan: authentik proxy provider, `forward_domain` mode, `cookie_domain:
bnei.dev`. One provider covers `fleet.bnei.dev` **and** every
`<id>-e2e.bnei.dev` preview. `forward_single` cannot — previews are minted at
runtime by `provisioner/internal/k8s/expose.go`, so no host exists to name up
front.

Changes here:
- `k8s/core.yaml` — swap `basic-admin-auth` for the forwardAuth middleware
- `expose.go:22` — same swap in `basicAuthMiddleware`, plus a second route
  rule: `PathPrefix('/outpost.goauthentik.io/')` → authentik. Sign-in
  callback lands on the *preview* host. Miss it, get a redirect loop that
  looks like broken auth, not a missing route.

Safe because the ingress only exposes port 8080 — SPA plus
`DashboardService`. Port 9090 (`CoreService`: sidecar, provisioner, wrapper)
has no `IngressRoute`. `/webhook/alertmanager` is called by Prometheus and
Grafana over the in-cluster Service URL. Checked both. No internal caller
breaks.

Traps found while scoping:
- authentik's embedded outpost has `providers = []`. A proxy provider does
  nothing until bound to an outpost.
- That list is **replaced, not appended**. So the whole forwardAuth tier goes
  in ONE blueprint file. One file per app, like the OIDC tier does it, means
  each blueprint silently unbinds the last.
- authentik Base URL setting was parked as a 2026.11 problem. Proxy providers
  build redirect URLs from it. Must be set first.

### 4.2 No native OIDC in core. On purpose.

Tempting. Skip it.

Fleet has no user model. No `users` table. Nothing keyed by identity. An OIDC
relying party in core would mint an identity nothing reads. forwardAuth plus
`X-authentik-*` headers gives the same attribution for none of the code.

Revisit only if fleet must be reached without Traefik in front — a CLI, a
mobile client.

### 4.3 Identity headers wait on #200

Outpost sets `X-authentik-username`, `-email`, `-groups`. Free attribution.
Sessions and proposals have no author today.

**Do not read them until #200 lands.** A worker pod can already POST to
`DashboardService` on the pod network behind only the `X-Fleet-Dashboard`
CSRF header. If core trusts an identity header, that pod can forge it. Hole
stops being "wrong authorization", becomes "impersonation". Worse.

Order: #200, then headers.

### 4.4 Cluster-side, still open

- default-deny NetworkPolicy per namespace, with carve-outs (CoreDNS,
  Prometheus scrape, Traefik → backend, authentik → Pigsty)
- PSA labels — every namespace runs `privileged` today
- ArgoCD `AppProject` — every Application is `project: default`, which permits
  any repo, any namespace, any cluster-scoped kind
- security alert rules — 401/403 spikes, audit authn failures, RBAC changes
- cert-expiry alert on `traefik_tls_certs_not_after` — this is the one control
  that catches every silent renewal failure above
- retire `basic-admin-auth` — spans three repos, must land in one go
- rotate `kubeadm_certificate_key.creds` — in git history
- five plaintext Pigsty DB passwords — rotation is circular, Infisical's own DB
  is on the list

---

## 5. Lessons. Read before debugging.

**Green is not proof.** Three separate times a step reported success while
doing nothing:
- Alloy loaded its audit-log component fine, aimed at the container path, not
  the host path. `local.file_match` on a missing path is not an error.
- authentik blueprint discovery was *enqueued* at worker boot and never ran.
  Log said `Task enqueued`, never `Task started`. No error anywhere.
- Infisical operator applied a CR whose template changed, and kept rendering
  the old Secret forever.

Ask "did the consuming step run", not "was my input accepted".

**Restarts are the propagation chain.** Nothing here hot-reloads.

```
merge                      -> ArgoCD applies the CR
restart infisical-operator -> only if the CR TEMPLATE changed
restart authentik worker   -> discovery re-applies the blueprint
restart the client app     -> only if ITS credentials changed (env vars)
```

Skip one, everything still looks healthy.

**Env vars bind at container start.** Rotate a secret, the running process
keeps the old one. Add `secrets.infisical.com/auto-reload: "true"`.

**Do not delete a managed Secret to force a re-render.** `creationPolicy:
Orphan` means the operator does not own it and will not recreate it. Watched
one stay gone 105s. Operator restart was needed anyway. Deleting is strictly
worse: same outcome, plus a window with a missing mount.

**Do not `cat` a mounted blueprint or `kubectl get secret -o yaml`.** Both
print live credentials. Leaked a Cloudflare token and a Grafana OIDC pair that
way; both rotated. Read the manifest in git, or use a jsonpath scoped to
`.metadata`.

**Helm eats unknown keys.** A misplaced key is a no-op for a whole session.
`helm template` and grep the rendered output. Same for YAML indentation — a
list nested two spaces wrong parsed clean and rendered zero entries.

**Test the negative.** A rate-limit test that passes under budget proves
nothing. An origin-lock test from the LAN either hairpins into the allowlist
or dies on NAT. Neither proves the lock works. Test from off-LAN, expect the
403.
