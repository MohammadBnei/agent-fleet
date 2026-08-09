# ADR-0031: Garage S3-backed shared file space, presigned URLs minted only by core

**Status:** Accepted
**Date:** 2026-08-08

## Context

Neither the human operator nor a worker agent had any way to exchange
arbitrary files with each other outside a task's own git worktree — no
drop space for references, assets, or artifacts that aren't meant to go
through a PR. `infra-bootstrap` already runs a Garage S3-compatible object
store (an off-cluster LXC, reachable in-cluster at `garage.bnei.lan:3900`
and externally at `https://s3.bnei.dev` via Traefik) already used by
Longhorn backups, pgBackRest, and the Ente app — but nothing in
`agent-fleet` touched it before this change.

This is a genuinely new kind of decision for this fleet on two axes:

1. **First fleet-owned dependency on an external service beyond
   GitHub/Anthropic.** Every existing external credential
   (`GH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `DISCORD_BOT_TOKEN`) is either
   injected directly into the one component that needs it, or (for
   Postgres) centralized in `core` alone (`adr/0020` point 1). A new
   external service is a new instance of that same "who holds the
   credential" question, not something either existing precedent
   automatically answers.
2. **First "core mints a capability for someone else to use directly"
   pattern.** Every other `core`-mediated capability today either stays
   entirely inside `core` (Postgres) or is a full proxy round-trip
   (`RequestE2eEnv` → provisioner → e2e pod). Presigned URLs are a third
   shape: core issues a short-lived credential-substitute, then steps out
   of the way for the actual data transfer.

## Decision

1. **`core` is the sole holder of the Garage credential** — mirrors the
   `AGENTFLEET_DB_*` centralization precedent (`adr/0020` point 1), not
   the `GH_TOKEN` direct-injection one. Rationale: unlike `GH_TOKEN` (every
   pod needs its own git identity to push as itself), there's no per-pod
   identity reason to hand out Garage credentials — one owner is strictly
   simpler and matches how this fleet already treats "a service credential
   shared by every component" (Postgres) versus "a credential that's
   inherently per-actor" (git commit identity, `adr/0006`).
2. **`core` never proxies file bytes — it only mints presigned PUT/GET
   URLs** (`GetFileUploadUrl`/`GetFileDownloadUrl`, both on `CoreService`
   for the agent side and `DashboardService` for the human side, sharing
   message shapes defined once in `proto/agentfleet/v1/files.proto`).
   Unary gRPC isn't built for arbitrary-size blobs, and every existing
   proxy in this fleet (`RequestE2eEnv`, `CallE2eTool`) proxies small
   JSON payloads, not file contents — a presigned URL keeps `core` in the
   same "broker, not conduit" role for something that genuinely doesn't
   fit the proxy shape.
3. **One flat namespace, not per-task.** A single bucket
   (`agent-fleet-files`), key = filename verbatim, last write wins. No new
   Postgres table — Garage's own `ListObjectsV2` is the listing source of
   truth, so there's nothing to keep in sync between two stores. Per-task
   scoping was considered and rejected: the actual use case (dropping a
   reference file, grabbing an asset) isn't naturally task-shaped the way
   a git worktree is, and a flat space is strictly less to build.
4. **Presigned URLs are always signed against the external endpoint**
   (`https://s3.bnei.dev`, `GARAGE_S3_ENDPOINT`), never the in-cluster
   `garage.bnei.lan` one. SigV4 signs the endpoint host into the request
   signature itself — a URL signed for one host doesn't validate against
   another. The dashboard's browser (outside the cluster) can't resolve
   `.bnei.lan` at all, and the worker pod's own `curl` can reach the
   external host over the same path Traefik already routes, so one
   endpoint config correctly serves both consumers instead of maintaining
   two signing paths for what's otherwise identical logic.
5. **Agents move bytes via `curl` from Bash, not a byte-carrying MCP
   tool.** The four new MCP tools (`list_shared_files`,
   `get_shared_file_upload_url`, `get_shared_file_download_url`,
   `delete_shared_file`, in `sidecar/internal/mcpserver`) only ever
   exchange JSON metadata with `core` — never file contents. The agent
   already has Bash and its own worktree filesystem; asking it to
   `curl -T <path> "<uploadUrl>"` reuses HTTP's native binary transport
   instead of inventing a base64-over-JSON transport for the same thing.
6. **No Discord surface.** Consistent with `adr/0029`'s "dashboard is
   primary, Discord is notification-only" — a `Files` page was added to
   the dashboard (`dashboard/src/pages/Files.tsx`, same list+action
   structure as `Worktrees.tsx`); no `/files` Discord command was added.

## Alternatives considered

- **Direct Garage credentials injected into the sidecar (and dashboard,
  via `core`)** — the `GH_TOKEN` precedent. Rejected: no per-pod identity
  reason exists for file access the way there is for git commits: every
  worker pod pushing under its own name is a real requirement, every pod
  reading/writing the same shared bucket under the same identity is not.
  A single credential holder is simply less surface.
- **Proxy file bytes through `core`'s gRPC** (agent → sidecar → core →
  Garage, mirroring `CallE2eTool`'s shape) — rejected: unary gRPC has
  practical message-size limits and this fleet's own proxied calls have
  never carried anything but small JSON payloads; a presigned URL avoids
  putting `core` in the data path for something with no natural size
  bound.
- **Per-task key prefix** (`tasks/<taskId>/...`) — rejected for v1: the
  motivating use case is general file exchange, not task-scoped artifact
  storage; a flat space is less to build and nothing currently needs the
  isolation a prefix would add. Revisit if collisions or cross-task
  leakage become a real problem.
- **A `list_shared_files` PLUS an `upload_file`/`download_file` MCP tool
  pair that actually carries bytes** (base64-encoded in the tool
  response) — rejected: Bash + `curl` already does this natively for
  anything already on the agent's own filesystem; duplicating that
  through MCP would only exist to avoid a two-step curl invocation.

## Consequences

- A new Go dependency (`aws-sdk-go-v2`, `core` only) — the first S3 client
  anywhere in this fleet's own code.
- Two new environment variables on `core` (`GARAGE_S3_ENDPOINT` default
  `https://s3.bnei.dev`, `GARAGE_FILES_BUCKET` default
  `agent-fleet-files`) plus two Infisical-sourced secrets
  (`AGENTFLEET_FILES_S3_ACCESS_KEY`/`_SECRET`), following the existing
  `<PREFIX>_S3_ACCESS_KEY`/`_SECRET` convention `docs/secrets.md`
  documents for every other Garage-backed app in `infra-bootstrap`.
- Bucket/key provisioning happens in `infra-bootstrap`, not here (a new
  `garage_buckets` entry in that repo's `garage-configure.yml`, run by a
  human per that repo's own "a human runs ansible personally" rule) — the
  agent-fleet side only consumes the resulting credential, same
  boundary every other cross-repo dependency in this fleet already
  respects.
- File deletion has no undo and no versioning — a `delete_shared_file`
  call (from either an agent or the dashboard) is immediate and final,
  same as the bucket's own S3 semantics. Acceptable for a general-purpose
  drop space; would need revisiting if this space ever holds anything
  that needs an audit trail.
