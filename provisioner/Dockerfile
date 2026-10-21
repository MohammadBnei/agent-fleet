# Build context must be the agent-fleet/ repo root (not provisioner/),
# since this needs go.work + proto/gen/go as siblings:
#   docker build -f provisioner/Dockerfile -t <tag> .
#
# No schema embed needed here (unlike core) — the provisioner holds no
# Postgres credentials at all (docs/adr/0020 point 1); Kubernetes itself is
# the source of truth for pod/session state.
FROM golang:1.27 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY provisioner ./provisioner
# go.work.sum: checksums only the workspace needs. Today that is core's
# google.golang.org/genproto (via golang-migrate); without the file every
# build asked sum.golang.org, and a lossy route to Google failed the image
# builds on 2026-10-20. The other images copy it for the same guarantee.
# Nothing checks it is current: a stale file only falls back to sumdb.
COPY go.work.sum ./
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists core/sidecar) isn't
# portable across a partial build context.
RUN go work init ./proto/gen/go ./provisioner
WORKDIR /app/provisioner
# No version compiled in: FLEET_VERSION at run time (see core/Dockerfile).
# Cache mounts: the Go build and module caches stay on the build box between
# builds (docker.yml), outside the image.
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build -o /out/provisioner ./cmd/provisioner

# Not distroless: as of docs/adr/0019, the provisioner owns the shared
# PVC's entire git lifecycle itself (clone/fetch/worktree add+remove,
# internal/git) — it shells out to `git` and `gh` directly, the same tools
# worker/Dockerfile already installs for the same reason (git.ts's
# configureGitAuth, ported here as git.Manager.ConfigureAuth).
FROM debian:12-slim
# Unpinned installs below: docker.yml passes the ISO week, so a cached layer
# is refreshed at least weekly instead of living forever.
ARG CACHE_WEEK=none
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates rsync \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/provisioner /provisioner
ENTRYPOINT ["/provisioner"]
