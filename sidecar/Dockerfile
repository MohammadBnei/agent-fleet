# Build context must be the agent-fleet/ repo root (not sidecar/), since
# this needs go.work + proto/gen/go as siblings:
#   docker build -f sidecar/Dockerfile -t <tag> .
#
# A genuine second container in the worker pod, not a process sharing the
# worker's own container (docs/adr/0020 point 5) — its own image, its own
# small footprint. No git/gh needed here (unlike provisioner): the sidecar
# only reads the already-prepared worktree to compute diff telemetry, it
# never clones or pushes.
FROM golang:1.27 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY sidecar ./sidecar
# go.work.sum: checksums only the workspace needs. Today that is core's
# google.golang.org/genproto (via golang-migrate); without the file every
# build asked sum.golang.org, and a lossy route to Google failed the image
# builds on 2026-10-20. The other images copy it for the same guarantee.
# Nothing checks it is current: a stale file only falls back to sumdb.
COPY go.work.sum ./
RUN go work init ./proto/gen/go ./sidecar
WORKDIR /app/sidecar
# Cache mounts: the Go build and module caches stay on the build box between
# builds, outside the image. See docker.yml for what that costs.
RUN --mount=type=cache,id=agent-fleet-gobuild,target=/root/.cache/go-build --mount=type=cache,id=agent-fleet-gomod,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -o /out/sidecar ./cmd/sidecar

FROM debian:12-slim
# Unpinned installs below: docker.yml passes the ISO week, so a cached layer
# is refreshed at least weekly instead of living forever.
ARG CACHE_WEEK=none
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/sidecar /sidecar
ENTRYPOINT ["/sidecar"]
