# Build context must be the agent-fleet/ repo root (not executor/):
#   docker build -f executor/Dockerfile -t <tag> .
FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY executor ./executor
# `go work init` with just these two modules, rather than COPYing the
# repo's own go.work — that file lists core/provisioner/sidecar/e2e-runner
# too, none of which are in this build context, and go refuses to load a
# workspace whose members are missing. Same pattern as sidecar/Dockerfile.
RUN go work init ./proto/gen/go ./executor
WORKDIR /app/executor
RUN CGO_ENABLED=0 go build -o /out/executor ./cmd/executor
# The kubectl shim ships in this same image so the provisioner's
# cluster-access ingredient can copy it into a session pod, and so shim
# and service are always the same build (docs/adr/0037).
RUN CGO_ENABLED=0 go build -o /out/kubectl-shim ./cmd/kubectlshim

# kubectl is the whole point of this image: the executor is the only
# process in the fleet holding cluster RBAC, and it reaches the API server
# as its own in-cluster ServiceAccount (auto-detected from the projected
# token mount — no kubeconfig, no credential outside the cluster).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/$(dpkg --print-architecture)/kubectl" -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/executor /usr/local/bin/executor
COPY --from=build /out/kubectl-shim /usr/local/bin/kubectl-shim
USER 65532:65532
CMD ["executor"]
