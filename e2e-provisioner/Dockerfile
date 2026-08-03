# Build context must be the agent-fleet/ repo root (not e2e-provisioner/),
# since this needs go.work + proto/gen/go as siblings:
#   docker build -f e2e-provisioner/Dockerfile -t <tag> .
#
# No schema embed needed here (unlike fleet-core) — e2e-provisioner never
# applies the schema itself, it only reads/writes e2e_sessions/tasks rows
# via the pool worker/fleet-core's migrate step already created.
FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY e2e-provisioner ./e2e-provisioner
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists fleet-core) isn't
# portable across a partial build context.
RUN go work init ./proto/gen/go ./e2e-provisioner
WORKDIR /app/e2e-provisioner
RUN CGO_ENABLED=0 go build -o /out/e2e-provisioner ./cmd/e2e-provisioner

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/e2e-provisioner /e2e-provisioner
ENTRYPOINT ["/e2e-provisioner"]
