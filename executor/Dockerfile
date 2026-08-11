# Build context must be the agent-fleet/ repo root (not executor/):
#   docker build -f executor/Dockerfile -t <tag> .
FROM golang:1.26 AS build
WORKDIR /src
# go.work resolves the local proto module, same as the other Go images.
COPY go.work go.work.sum ./
COPY proto/gen/go ./proto/gen/go
COPY executor ./executor
RUN cd executor && CGO_ENABLED=0 go build -o /out/executor ./cmd/executor

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
USER 65532:65532
CMD ["executor"]
