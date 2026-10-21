# Build context must be the agent-fleet/ repo root (not core/), since
# this needs go.work + proto/gen/go as siblings:
#   docker build -f core/Dockerfile -t <tag> .

# Built ahead of the Go stage: go:embed reads internal/webui/dist at Go
# build time, so the SPA's output has to exist before `go build` runs (see
# docs/adr/0014).
FROM oven/bun:1-slim AS spa
WORKDIR /app/dashboard
COPY dashboard/package.json dashboard/bun.lock ./
RUN --mount=type=cache,id=agent-fleet-bun,target=/root/.bun/install/cache bun install --frozen-lockfile
COPY dashboard/ ./
RUN bun run build

# The islands (docs/adr/0068): web components host pages load from
# /islands/*. They import the generated AgentService code and the shared
# transcript helpers from dashboard/src, at the same relative paths as in the
# repo. Type checks run in CI's islands-test; the image only bundles.
FROM oven/bun:1-slim AS islands
WORKDIR /app/islands
COPY islands/package.json islands/bun.lock ./
RUN --mount=type=cache,id=agent-fleet-bun,target=/root/.bun/install/cache bun install --frozen-lockfile
COPY dashboard/src/gen /app/dashboard/src/gen
COPY dashboard/src/transcript.ts /app/dashboard/src/transcript.ts
COPY islands/ ./
RUN bun x vite build

FROM golang:1.27 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY core ./core
COPY --from=spa /app/dashboard/dist ./core/internal/webui/dist
COPY --from=islands /app/islands/dist ./core/internal/islands/dist
# go.work.sum: checksums only the workspace needs. Today that is core's
# google.golang.org/genproto (via golang-migrate); without the file every
# build asked sum.golang.org, and a lossy route to Google failed the image
# builds on 2026-10-20. The other images copy it for the same guarantee.
# Nothing checks it is current: a stale file only falls back to sumdb.
COPY go.work.sum ./
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists provisioner/sidecar)
# isn't portable across a partial build context.
RUN go work init ./proto/gen/go ./core
WORKDIR /app/core
# No version compiled in: core reads FLEET_VERSION at run time, so one image
# serves every release that doesn't change its inputs (docker.yml).
# Cache mounts: the Go build and module caches stay on the build box between
# builds, outside the image. See docker.yml for what that costs.
RUN --mount=type=cache,id=agent-fleet-gobuild,target=/root/.cache/go-build --mount=type=cache,id=agent-fleet-gomod,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/core /core
ENTRYPOINT ["/core"]
