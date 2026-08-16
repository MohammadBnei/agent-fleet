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
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists provisioner/sidecar)
# isn't portable across a partial build context.
RUN go work init ./proto/gen/go ./core
WORKDIR /app/core
# The version the dashboard reports for core (and for the SPA embedded above,
# which has no version of its own). docker.yml passes the same string it tags
# the image with, so the binary and the tag cannot drift. Nothing else can
# supply it: this context has no .git, so debug.ReadBuildInfo has no VCS stamp.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/core ./cmd/core

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/core /core
ENTRYPOINT ["/core"]
