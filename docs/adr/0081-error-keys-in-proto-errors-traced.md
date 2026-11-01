# ADR-0081 — Error keys are declared in each service's proto; an error is traced by its trace id

- **Status:** Accepted (2026-10-28)
- **Built:** §5 steps 1–3 but the parse removal: core requires `ukubi-go` v0.2.0 and
  `core/internal/errkeys` is its wrapper (one `connectkeys.Keys`, template
  `agentfleet.v1.ErrorKey`, the registry constants); the `Meta()` race is gone and
  `connectkeys/wiring` is `TestWiring`. §1–§3: `agent.proto` declares `AgentErrorKey`
  (`agent.definition_changed`, `agent.thread_cap`, `agent.definition_unavailable`) and
  each RPC's `(ukubi.errors.v1.keys)`, from `proto/ukubi/errors/v1/options.proto`
  vendored from v0.2.0; `TestDeclared` runs `connectkeys.CheckDeclared` over
  AgentService. The islands' `errors.ts` and the dashboard's `agentErrors.ts` map the
  keys to text, typed on the generated enum's names. Three keys, not about 12: §2's test
  leaves only these with an islands or dashboard reader for whom the code is ambiguous
  (the owner-only, client-only and origin refusals reach neither, and every `Aborted`
  means retry). The taint refusals get no key and stay an unkeyed `PermissionDenied`:
  [ADR-0082](0082-decision-cards.md) §3 removes taint.
  §4 (§5 step 4, 2026-10-31): core mints a span on every Connect call (DashboardService,
  AgentService on 8080 and 9091) and every `CoreService` call, through `ukubi-go`
  telemetry (`Interceptors`, `ServerOption`, `WithExporter(nil)` when no endpoint is set),
  and exports them only to `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`. Every Connect response
  carries a `Trace-Id` header, every `CoreService` response a `trace-id` trailer; the
  existing access-log lines (`dashboard rpc`, `core grpc`, INFO, words unchanged) gain
  `trace_id`, `code`, `key` and `source`, the `file:line` that `errkeys.Connect` recorded
  (core's `connect.NewError` sites now build through it; gRPC errors have no source).
  All logs go through `telemetry.NewLogger`. The console's error text appends the trace id.
  Gaps: ARCHITECTURE.md §5 Observability.
  **Not yet built:** the `rawMessage.includes("DEFINITION_CHANGED")` removal (§5 step 3)
  and §5 step 5.
- **Date:** 2026-10-28
- **Related:** [ADR-0066](0066-agent-service.md) (AgentService),
  [ADR-0067](0067-shared-and-private-sessions.md) (who may do what in a thread),
  [ADR-0068](0068-islands.md) (the islands), [ADR-0015](0015-connectrpc-dashboard-api.md)
  (the dashboard's ConnectRPC client), `proto/agentfleet/v1/errors.proto`,
  `core/internal/errkeys`

## Context

Core attaches an `agentfleet.v1.ErrorKey` detail (a `key` string and a `source`
string) to its Connect and gRPC errors. `core/internal/errkeys` holds the keys as
Go constants, `TestErrKeys` refuses a key that is not one of them, and an
interceptor gives every unkeyed error `server.internal` with its code and words
unchanged. Two keys exist: `server.internal` and `agent.definition_changed`. The
package comment promises that the specific keys "arrive package by package", so
that in the end every site has one.

What the clients do with that today:

- **One code, many meanings.** The islands turn every `PermissionDenied` into
  "if it holds untrusted input, only an admin can answer it"
  (`islands/src/errors.ts:51-52`). AgentService returns `PermissionDenied` from 9
  sites: an actor outside a client's `actors.allow`, a member calling a
  client-only method and the reverse, a non-owner changing mode, config or stopping
  a thread, posting or answering where one may not, and an origin refusal. Only one
  of those is about untrusted input.
- **The wrong cause.** The islands turn `ResourceExhausted` into "the agent is busy
  with another session" (`errors.ts:47-48`). AgentService returns it only for the
  open-thread caps (`core/internal/agentsvc/threads.go:196`,
  `core/internal/agentsvc/open.go:178`). "Busy" (too many requests in progress, a
  claim in flight) is `Aborted` (`open.go:123`).
- **A message parse.** Staleness is still read from the words for one more release:
  `rawMessage.includes("DEFINITION_CHANGED")` (`errors.ts:39`).
- **The registry is hand-kept Go.** Clients get no types from it, so nothing tells
  the islands or the dashboard that a key exists or went away, and `buf` cannot
  guard it.
- **A data race.** The fallback interceptor copies a `*connect.Error` before keying
  it, because handlers return shared sentinels such as `errNoThread`
  (`agentsvc/threads.go:32`). The copy reads `ce.Meta()` on the original
  (`core/internal/errkeys/errkeys.go:124`), and connect-go's `Meta()` allocates
  the map on first call. That is a write to the shared sentinel, from concurrent
  requests.

`ukubi-go` (the shared Go module) is getting `errs/connectkeys`: the same detail,
built from a single template message, with the copy done before any write.

A key answers "what happened, so the client can act". The other question, "where
did it come from and why", is asked by whoever debugs an error, and a key per site
would answer it badly: about 213 `connect.NewError` calls in core, each needing a
name that a stack location already gives.

## Decision

### 1. Keys are part of each service's proto contract

- **A per-service enum.** Each service that has keys declares them in its own proto,
  e.g. `AgentErrorKey`. Each value carries its wire string through an enum value
  option from `ukubi.errors.v1`. The wire stays `ErrorKey.key`, a string, so the
  bytes a client receives do not change. The enum is the declaration; the string is
  what travels.
- **A method option.** `(ukubi.errors.v1.keys)` on each RPC lists the keys that RPC
  may return.
- Both options come from `ukubi-go` v0.2.0 (not released yet).

### 2. A key exists only where the code alone does not say what happened

A site gets a key only when a client (the dashboard or the islands) must act on the
error or show text for it, **and** the code alone is ambiguous. Every other site
stays `server.internal`, with its specific code and its words.

- Expect about 12 AgentService keys, from the `PermissionDenied`,
  `ResourceExhausted` and `Aborted` sites above, plus the existing
  `agent.definition_changed`.
- This deliberately reverses the errkeys comment's promise of a key at every site.
  The comment is rewritten in this PR.

### 3. Enforcement uses stock tooling only

`buf`, `protoc-gen-go` and `protoc-gen-es`. No custom plugin, and no reflection at
run time.

- **One Go test** walks the descriptors with `protoreflect` and refuses an
  undeclared key (an enum value with no wire string), a wire string used twice, and a
  method listing a key that is missing from the service's enum.
- **`buf breaking`** (already in CI, `PACKAGE` rules) refuses a renamed or removed
  enum value.
- **The TS translation maps** in the islands and the dashboard are typed
  `Record<AgentErrorKey, string>`, so a key with no text is a `tsc` error.

### 4. "Where from and why" is answered by tracing, not keys

- Every error response carries its trace id.
- Core logs each handler error once, as JSON, with `trace_id`, `key`, `code` and
  `source` (`file:line`). Loki can already search that by trace id; a trace backend
  is deferred.
- This is track 4's part (§5 step 4).
- **§4 amended 2026-10-31 (owner): core always mints spans; export only with
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`.** The ukubi Code Standard §7 has an app without
  an endpoint trace nothing; core keeps the ids without a backend, so the trace id on a
  response and on its log line exists from day one. No new log line: the access-log
  line already written per call carries the fields, at its own level, so no new ERROR
  series feeds the Loki alerts.

### 5. Order

1. **After `ukubi-go` v0.1.0:** replace `core/internal/errkeys` with `ukubi-go`
   `errs/connectkeys`, using the single template `agentfleet.v1.ErrorKey`. The wire
   does not change. This also fixes the `Meta()` race: `connectkeys` copies the
   error before touching it. `connectkeys/wiring` replaces `TestWiring`.
2. **`ukubi-go` v0.2.0:** the two options and the descriptor test helper.
3. **Fleet:** the `AgentErrorKey` enum and its keys; then the islands' and the
   dashboard's translation maps; then removal of the
   `rawMessage.includes("DEFINITION_CHANGED")` parse (`errors.ts:39`).
4. **Track 4:** spans, the trace id on error responses, and the error log line.
5. **Later:** emit both ErrorKey details, `agentfleet.v1.ErrorKey` and
   `ukubi.errors.v1.ErrorKey`, with identical fields. The islands first accept both
   type names; `agentfleet.v1` is dropped one release after islands that read the
   new name are live. That PR is labelled a wire behaviour change.

### 6. Out of scope

- **Plain `http.Error` sites** (`core/internal/auth/oidc.go`, `gate.go`,
  `cors.go`, `core/internal/alertwebhook`): they answer before Connect does, and
  get no keys.
- **Core's gRPC errors to the sidecar** (`CoreService`): the sidecar branches only
  on `Canceled` and `DeadlineExceeded`
  (`sidecar/internal/mcpserver/server.go:495`).
- **The trace backend.**

## Alternatives considered

- **A key at every site** (about 213 Connect constructors). Upkeep for every new
  error, and it duplicates what a trace and a `file:line` already say.
- **Keys only where a client branches today.** Misses the codes that already carry
  several meanings (`PermissionDenied`), which is where the islands are wrong now.
- **Keep the hand-kept Go registry and `TestErrKeys`.** `buf` cannot guard it, and
  clients get no types.
- **A custom codegen plugin** for per-RPC TS unions. A plugin to maintain, for little
  over `Record<Enum, string>` plus the method option.
- **`google.rpc.ErrorInfo` (AIP-193).** ukubi already has a byte-compatible
  `ErrorKey`; a second shape buys nothing.
- **One spelling of "internal".** `ukubi-go` keeps `connectkeys.KeyInternal`
  (`server.internal`, Connect) and `errs.KeyInternal` (`internal`, JSON bodies)
  apart on purpose; the fleet uses the Connect one.

## Consequences

- A client can say what actually happened for the codes that mean several things,
  and a key that disappears breaks `buf breaking` or `tsc` instead of silently
  falling back to a wrong sentence.
- Most errors stay `server.internal`. Debugging one means its trace id, not its key.
- The islands read the detail by hand today and do not import generated code
  (`errors.ts`, top). Typing their map on the enum means importing the generated
  enum there.
- **Reversibility.** Each key is a one-way door once a client reads it; `buf`
  guards it. The approach is a two-way door: the wire string is unchanged, so
  moving the declaration back to Go would change no bytes.
