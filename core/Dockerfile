# Build context must be the agent-fleet/ repo root (not core/), since
# this needs go.work + proto/gen/go as siblings:
#   docker build -f core/Dockerfile -t <tag> .

# Built ahead of the Go stage: go:embed reads internal/webui/dist at Go
# build time, so the SPA's output has to exist before `go build` runs (see
# docs/adr/0014).
FROM oven/bun:1-slim AS spa
WORKDIR /app/dashboard
COPY dashboard/package.json dashboard/bun.lock ./
RUN bun install --frozen-lockfile
COPY dashboard/ ./
RUN bun run build

FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY core ./core
COPY --from=spa /app/dashboard/dist ./core/internal/webui/dist
# db/schema.sql copied in as a build-time embed source — go:embed can't
# reach outside its own package directory, so the canonical file
# (db/schema.sql) stays the single source of truth; this copy is what
# `core migrate` actually applies. See internal/db/migrate.go.
COPY db/schema.sql ./core/internal/db/embedded_schema.sql
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists provisioner/sidecar)
# isn't portable across a partial build context.
RUN go work init ./proto/gen/go ./core
WORKDIR /app/core
RUN CGO_ENABLED=0 go build -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/core /core
ENTRYPOINT ["/core"]
