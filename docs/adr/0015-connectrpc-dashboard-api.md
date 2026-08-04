# ADR-0015: ConnectRPC replaces the dashboard's REST+SSE API

**Status:** Accepted
**Date:** 2026-08-04

## Context

ADR-0014 gave the web dashboard a working backend: hand-rolled REST routes
and an SSE stream in `fleet-core/internal/dashboard/`, consumed by a
hand-written `fetch`/`EventSource` client in `dashboard/src/api.ts`. That
left three independently-maintained copies of the same shapes — the Go
structs (`tasks.Task`, `transcript.Entry`), the TS interfaces, and the
seven route contracts — with nothing keeping them in sync but discipline.

`proto`/`buf` is already this repo's pattern for its one real internal
gRPC call (`e2e.proto`, `fleet-core` → `e2e-provisioner`, per ADR-0013).
This decision extends that pattern to the dashboard: one buf-defined
service, one generated Go server, one generated TypeScript client.

This was designed through an `architecture-interview`, then stress-tested
with an adversarial `doubt-driven-development` pass before landing on the
design below. That review caught a hard compile-time collision the first
draft missed — `proto/agentfleet/v1/transcript.proto` already defines
`TranscriptEntry`, placed there by ADR-0013 specifically anticipating "a
non-MCP client" someday needing real codegen. This is that client; the new
service reuses that message (and `ReadTranscriptSinceRequest`/
`ReadTranscriptSinceResponse`) via a proto import instead of duplicating
them.

**This decision explicitly supersedes two specific lines in ADR-0013:**

> "fleet-core exposing its own gRPC server defensively, for later —
> deferred, not built: no real second internal client of fleet-core exists
> yet; add it when one does."

> "connect-es/@bufbuild/protobuf-es for the TS side — rejected: worker
> never speaks gRPC/Connect on any wire; a full RPC-client codegen for
> data that only ever travels as MCP JSON would be dead code."

Both were true when written. The premise has changed: the dashboard's TS
client is now that second caller, and it needs exactly the RPC-client
codegen the first rejection was about — just for the dashboard, not
`worker` (which still speaks MCP-over-HTTP only, unchanged by this
decision).

ADR-0014's other content is unaffected and is referenced here, not
re-derived: BasicAuth at the ingress layer, write actions calling the
same store methods Discord's commands call, and idempotency-key reuse.

## Decision

`fleet-core/internal/dashboard/` is rewritten as a buf-defined
`DashboardService` (`proto/agentfleet/v1/dashboard.proto`), served via
`connect-go` on the same `http.ServeMux` `run.go` already owns, replacing
the REST+SSE handlers outright — full cutover, no transition period,
matching ADR-0013's own big-bang precedent (no live traffic or other
consumers of the old routes to protect at solo-operator scale).
`dashboard/`'s hand-written `fetch`/`EventSource` client
(`dashboard/src/api.ts`) is replaced by a generated TypeScript client
(`@connectrpc/connect` + `@connectrpc/connect-web`).

The live-transcript feed becomes a Connect **server-streaming RPC**
(`StreamTranscript`), not a converted-but-still-separate SSE route —
one transport for everything, not two. `fleet-core/internal/dashboard/
hub.go`'s shared-poller design (one poller per subscribed task, not one
per open connection) is reused almost unchanged; only its channel/value
type moves from a local `Event` struct to `*agentfleetv1.TranscriptEntry`
pointers, since generated protobuf structs are pointer-idiomatic and
`go vet`'s copylocks check flags value copies of them.

**Codegen, verified against the actual ecosystem rather than assumed:**
Go uses `protoc-gen-connect-go` (a local plugin, matching this repo's
existing local-plugins-only convention — no BSR/remote plugins anywhere in
`buf.gen.yaml`). TypeScript uses **`protoc-gen-es` alone** — Connect-ES
v2 merged service-descriptor generation into `protoc-gen-es` itself
(`GenService`, consumed directly by `@connectrpc/connect`'s `createClient`);
the separate `protoc-gen-connect-es` plugin is the older v1 API and
produces an incompatible descriptor shape. This was discovered empirically
mid-implementation (a first attempt with both plugins installed produced a
type error at the `createClient` call site) and is worth recording here so
it isn't rediscovered the same way later.

**CSRF mitigation moves to interceptors**, replacing per-route middleware:
a `connect.Interceptor` server-side (`fleet-core/internal/dashboard/
interceptor.go`) checking `X-Fleet-Dashboard` in both `WrapUnary` and
`WrapStreamingHandler`, and a `connect-web` transport interceptor
client-side (`dashboard/src/connectClient.ts`) setting it on every call —
reads included, not just writes, since one interceptor is simpler than
conditional per-method logic. The threat model is unchanged from ADR-0014:
BasicAuth credentials are auto-attached by browsers to same-origin
requests regardless of which page triggered them, so a required custom
header (unsettable by a plain form/img, and blocked by CORS for any
cross-origin `fetch` that tries) is what actually stops forgery.

**Client-side reconnect is added, not silently dropped.** Native
`EventSource` auto-reconnects on a dropped connection; a `for await` loop
over a Connect stream does not. Since this whole system's pull/cursor
design (ADR-0013) exists specifically so a reconnect can resume without
loss, `dashboard/src/connectClient.ts`'s `subscribeTranscript` wraps the
streaming call in a retry loop that resubscribes from the last-received
`seq` on any drop — reproducing the property `EventSource` gave for free,
rather than regressing it.

**Precise wording for the "gRPC-capable" consequence:** `connect-go`'s
handler understands Connect, gRPC, and gRPC-Web wire framing on one mux
path, with no simple option found to restrict it to Connect-protocol-only.
However, `run.go`'s plain `http.Server` (no `h2c`, no TLS+ALPN) still
cannot actually serve a real gRPC-over-HTTP/2 client today — the honest
description is "protocol-capable," not "a working gRPC endpoint." The
practical exposure added is the Connect protocol and gRPC-Web, both
already behind the same BasicAuth-gated host ADR-0014 accepted for `/mcp`.

## Alternatives considered

- **Share proto message definitions without adopting Connect's RPC
  framework** (hand-keep `fetch`/`EventSource` calling shared-shape JSON)
  — rejected: solves type duplication but not the actual duplication that
  matters, the hand-maintained calling code in `dashboard/src/api.ts`.
- **Run the old REST+SSE surface and the new Connect API side by side,
  delete REST later** — rejected: no live traffic or other consumers to
  protect, so a transition period buys nothing (same reasoning ADR-0013
  used for its own cutover).
- **Keep SSE hand-rolled, only convert the six unary routes** — rejected:
  leaves two transports and two client patterns in the SPA instead of one.
- **Restrict `connect-go`'s handler to Connect-protocol-only** — no
  built-in server option found; would require custom Content-Type
  filtering middleware. Not pursued: the added surface is already behind
  the same BasicAuth wall accepted for `/mcp`, marginal not new in kind.
- **Define a new `TranscriptEntry`-equivalent message for the dashboard**
  instead of reusing `transcript.proto`'s — rejected by the doubt-driven
  review: a hard duplicate-symbol collision in the same `agentfleet.v1`
  package, and exactly the "non-MCP client" ADR-0013 built the existing
  message for.

## Consequences

- `fleet-core` is a real gRPC-capable server for the first time (see the
  precise wording above), reachable via its public dashboard host. This
  reopens and updates ADR-0013's "no gRPC server" line.
- `dashboard/`'s build gains generated code (`dashboard/src/gen/`) that
  must be regenerated and committed whenever `dashboard.proto` changes —
  CI's `buf generate` drift check (`.github/workflows/go.yml`) now also
  diffs `dashboard/src/gen`, not just `worker/src/gen`.
- `dashboard/tsconfig.app.json`'s `erasableSyntaxOnly` (a Vite scaffold
  default, not a deliberate project convention) had to be turned off:
  `protoc-gen-es`'s generated `TranscriptEntryType` is a real TypeScript
  `enum`, which that option rejects.
- The hand-written `fetch`/`EventSource` client code and the REST/SSE Go
  handlers are deleted outright, not deprecated in place — there is no
  fallback path if the Connect API has a problem post-deploy.
- Bundled with this migration: a pre-existing, unrelated bug in
  `TaskDetail.tsx` was fixed (an unconditional loading-state early return
  made its error banner unreachable on any fetch failure) — caught by the
  same doubt-driven review, fixed in the same rewrite since the component's
  data-fetching logic was being replaced anyway.
