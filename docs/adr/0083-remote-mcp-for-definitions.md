# ADR-0083 — A definition may name remote MCP servers (proposed, waiting for a user)

- **Status:** Proposed (2026-11-15). Waiting for a named user: no definition asks for a
  remote MCP server today.
- **Date:** 2026-11-15
- **Built:** nothing.
- **Would amend:** [ADR-0048](0048-one-session-one-pod-one-shared-home.md) §6 ("MCP is
  local, full stop"), [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §5
  ("any MCP server that needs the network" breaks in a definition pod, deliberately).
- **Related:** [ADR-0062](0062-defined-agents.md) (the definition spec and its ceiling),
  [ADR-0067](0067-shared-and-private-sessions.md) §4 (the definition tool surface),
  [ADR-0075](0075-github-app-identity-and-vault-credentials.md) (credentials from the
  vault), [ADR-0082](0082-decision-cards.md) §5 (grants).

## Context

kagent (kagent-dev/kagent, v1.0.1, 2026-11-12) lets an agent use tools from a remote MCP
server. A `RemoteMCPServer` object names the server (`url`, headers from secrets, timeouts,
TLS) and lists the tools it found. An agent binds those tools with
`tools[].mcp.{server, tools[], requireApproval}`.

The fleet has nothing like it, on purpose. The agent reaches only its own pod's sidecar
and the local Playwright server (`worker/src/session.ts`, `mcpServers`). A definition pod's
egress lock lets nothing out but the vault proxy, core and DNS. An `mcp__<other>__*` call
in a definition session is denied by `definitionDecision`, because it is outside the
built-in ceiling.

This ADR was written during a 2026-11-15 review of kagent. The owner chose to record the
shape and wait: none of the five definitions (`agent-reviewer-agent-fleet`,
`dev-dream-analyst`, `dev-notes`, `opus-workspace`, `workspace-base`) has a server to name.
Opening the door with no user would loosen two accepted ADRs for nothing.

## Decision (when a user arrives)

### 1. Definitions only

The `mcpServers` field exists only in an `AgentDefinition`. Repo sessions stay as they
are. A repo pod's egress is open anyway (ADR-0075 §7), and its MCP is the repo's own
business, gated by `canUseTool`.

### 2. The spec

```yaml
mcpServers:
  - name: linear            # [a-z0-9-]+, not agent-fleet-sidecar or playwright
    url: https://mcp.linear.app/mcp
    tools: [list_issues, create_issue]   # explicit, never a wildcard
    approval: [create_issue]             # subset of tools; these ask a human
```

The loader (`core/internal/definitions/load.go`) refuses:
- a URL that is not `https`;
- a reserved or duplicate name;
- an empty `tools` list or a wildcard;
- an `approval` entry that is not in `tools`.

### 3. Credentials through the vault, never direct

The server's host is a `credentials.services` entry like any other: the vault holds the
token and the pod holds a placeholder. The MCP connection goes through the vault proxy,
and `unmatched_host_policy=deny` stays. The CiliumNetworkPolicy does not change, because
traffic already reaches the vault proxy. Unverified: Claude Code's MCP HTTP client
honours `HTTPS_PROXY` and the vault CA bundle. The first slice proves this before
anything else.

### 4. The tool gate

- **Listed and not in `approval`:** added to `allowedTools` as `mcp__<name>__<tool>`.
- **In `approval`:** goes to the existing decision flow, including ADR-0082 grants.
- **Anything else on that server:** stays denied.

`FLEET_OWN_TOOL` (the always-allowed fleet MCP tools) does not change. A remote server's
tools are never the fleet's own.

## Alternatives considered

- **A `RemoteMCPServer` object shared across definitions (kagent's shape).** Rejected for
  now. There is one operator and no sharing need, and the CRD and operator path is closed
  (ADR-0022, ADR-0034). A name inside the reviewed definition file is enough.
- **Remote MCP through the sidecar as a proxy.** Not needed: the vault proxy already is
  the one egress path and holds the credential.
- **Build it now.** Rejected by the owner on 2026-11-15: no named user.

## Consequences

- ADR-0048 §6 becomes "MCP is local, except servers a reviewed definition names". The
  sentence in `ARCHITECTURE.md` and `CLAUDE.md` changes with it.
- ADR-0065 §5's "deliberately breaks" list loses its MCP entry for named servers only.
- A remote server can return text that steers the agent. The tool list and the approval
  subset bound what the agent can *do* with it, not what it reads.

## The bet

A named, reviewed list of tools behind the vault keeps the definition pod's authority as
narrow as it is today. Revisit if a user needs tool discovery, which kagent reports in
`status.discoveredTools`.

## Revisit when

A definition needs a tool that only a remote MCP server provides, and the owner names it.
That definition and server are then this ADR's first slice.
