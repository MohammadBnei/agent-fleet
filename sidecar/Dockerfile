# Build context must be the agent-fleet/ repo root (not sidecar/), since
# this needs go.work + proto/gen/go as siblings:
#   docker build -f sidecar/Dockerfile -t <tag> .
#
# A genuine second container in the worker pod, not a process sharing the
# worker's own container (docs/adr/0020 point 5) — its own image, its own
# small footprint. No git/gh needed here (unlike provisioner): the sidecar
# only reads the already-prepared worktree to compute diff telemetry, it
# never clones or pushes.
FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY sidecar ./sidecar
RUN go work init ./proto/gen/go ./sidecar
WORKDIR /app/sidecar
RUN CGO_ENABLED=0 go build -o /out/sidecar ./cmd/sidecar

FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/sidecar /sidecar
ENTRYPOINT ["/sidecar"]
