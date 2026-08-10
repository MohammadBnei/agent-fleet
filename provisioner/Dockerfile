# Build context must be the agent-fleet/ repo root (not provisioner/),
# since this needs go.work + proto/gen/go as siblings:
#   docker build -f provisioner/Dockerfile -t <tag> .
#
# No schema embed needed here (unlike core) — the provisioner holds no
# Postgres credentials at all (docs/adr/0020 point 1); Kubernetes itself is
# the source of truth for pod/session state.
FROM golang:1.26 AS build
WORKDIR /app
COPY proto/gen/go ./proto/gen/go
COPY provisioner ./provisioner
# A fresh, minimal go.work scoped to only what's copied into this image —
# the repo-root go.work (dev convenience, also lists core/sidecar) isn't
# portable across a partial build context.
RUN go work init ./proto/gen/go ./provisioner
WORKDIR /app/provisioner
RUN CGO_ENABLED=0 go build -o /out/provisioner ./cmd/provisioner

# Not distroless: as of docs/adr/0019, the provisioner owns the shared
# PVC's entire git lifecycle itself (clone/fetch/worktree add+remove,
# internal/git) — it shells out to `git` and `gh` directly, the same tools
# worker/Dockerfile already installs for the same reason (git.ts's
# configureGitAuth, ported here as git.Manager.ConfigureAuth).
FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git curl ca-certificates rsync \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/provisioner /provisioner
ENTRYPOINT ["/provisioner"]
