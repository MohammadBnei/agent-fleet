package k8s

import (
	"fmt"
	"os"
	"strings"
)

const (
	AppPort         = 3000
	CodeServerPort  = 8080
	PlaywrightPort  = 8931
	ComponentLabel  = "agent-fleet.dev/component"
	TaskIDLabel     = "agent-fleet.dev/task-id"
	ComponentE2eRun = "e2e-runner"
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
	case "dream-analyst", "vos-monolith":
		return "bun install && bun run dev", nil
	default:
		return "", fmt.Errorf("no e2e start command configured for repo %q", repo)
	}
}

// WorkspacePvcFor mirrors workspacePvcFor: common-app-chart's pvc.yaml
// names the PVC after the Release name ("<repo>-worker").
func WorkspacePvcFor(repo string) string {
	return repo + "-worker"
}

// ResourceName mirrors resourceName: short, DNS-safe, deterministic.
func ResourceName(taskID string) string {
	name := strings.ReplaceAll(taskID, "-", "")
	if len(name) > 20 {
		name = name[:20]
	}
	return "e2e-" + name
}

func PlaywrightURLFor(namespace, taskID string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/mcp", ResourceName(taskID), namespace, PlaywrightPort)
}

func PreviewURLFor(host, taskID string) string {
	return fmt.Sprintf("https://%s/%s/app/", host, taskID)
}

func Labels(taskID string) map[string]string {
	return map[string]string{ComponentLabel: ComponentE2eRun, TaskIDLabel: taskID}
}
