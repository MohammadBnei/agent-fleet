package k8s

import (
	"fmt"
	"os"
	"strings"
)

const (
	AppPort        = 3000
	CodeServerPort = 8080
	PlaywrightPort = 8931
	// ExecPort is the e2e pod's first-party run_command MCP listener
	// (execmcp) — distinct from PlaywrightPort's third-party server, which
	// we can't add tools to.
	ExecPort = 8932
	// SidecarMCPPort is agent-facing (local MCP server, the Agent SDK's
	// mcpServers config), SidecarAPIPort is wrapper-facing (plain local
	// HTTP/JSON, docs/adr/0020 point 5's two local surfaces).
	SidecarMCPPort = 9090
	SidecarAPIPort = 9091
	ComponentLabel = "agent-fleet.dev/component"
	TaskIDLabel    = "agent-fleet.dev/task-id"
	// RepoLabel lets TearDownSession recover which repo a worker pod
	// belongs to (needed to remove its worktree) from the pod itself —
	// the provisioner holds no DB credentials to look it up any other way
	// (docs/adr/0020 point 1), so Kubernetes has to carry it.
	RepoLabel       = "agent-fleet.dev/repo"
	ComponentE2eRun = "e2e-runner"
	ComponentWorker = "worker"
)

// StartCmdFor mirrors REPO_START_CMD in the TS k8s.ts: a per-repo lookup,
// env-overridable via E2E_START_CMD_<REPO> (upper-snake-cased), not a
// config file — two repos today, not worth more than this.
func StartCmdFor(repo string) (string, error) {
	envVar := "E2E_START_CMD_" + strings.ToUpper(strings.ReplaceAll(repo, "-", "_"))
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	switch repo {
	// dream-analyst's Bun/SvelteKit app lives under front/, not the
	// worktree root (confirmed live: "bun install" at the root fails with
	// "could not find a package.json file to install from" — the repo
	// root only has compose.yml/helm/front, no package.json).
	case "dream-analyst":
		return "cd front && bun install && bun run dev", nil
	case "vos-monolith":
		return "bun install && bun run dev", nil
	default:
		return "", fmt.Errorf("no e2e start command configured for repo %q", repo)
	}
}

// ResourceName mirrors resourceName: short, DNS-safe, deterministic.
func ResourceName(taskID string) string {
	return "e2e-" + shortID(taskID)
}

// WorkerResourceName is ResourceName's worker-pod counterpart — a distinct
// prefix so a worker pod and an e2e-preview pod for the same task never
// collide on one Pod name.
func WorkerResourceName(taskID string) string {
	return "worker-" + shortID(taskID)
}

func shortID(taskID string) string {
	name := strings.ReplaceAll(taskID, "-", "")
	if len(name) > 20 {
		name = name[:20]
	}
	return name
}

func PlaywrightURLFor(namespace, taskID string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/mcp", ResourceName(taskID), namespace, PlaywrightPort)
}

func ExecURLFor(namespace, taskID string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/mcp", ResourceName(taskID), namespace, ExecPort)
}

func PreviewURLFor(host, taskID string) string {
	return fmt.Sprintf("https://%s/%s/app/", host, taskID)
}

func Labels(taskID string) map[string]string {
	return map[string]string{
		ComponentLabel: ComponentE2eRun,
		TaskIDLabel:    taskID,
		"app":          "e2e-" + shortID(taskID),
	}
}

func WorkerLabels(taskID, repo string) map[string]string {
	return map[string]string{
		ComponentLabel: ComponentWorker,
		TaskIDLabel:    taskID,
		RepoLabel:      repo,
		// Required by the e2e-runner NetworkPolicy's rule 2 (agent-fleet.dev
		// part-of selector) — without it, worker pods silently lose access
		// to their own e2e-preview pod's app port for API testing.
		"app.kubernetes.io/part-of": "agent-fleet",
	}
}
