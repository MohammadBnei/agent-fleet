package k8s

import (
	"fmt"
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
	SSHPort  = 2222
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
	RepoLabel               = "agent-fleet.dev/repo"
	ComponentE2eRun         = "e2e-runner"
	ComponentWorker         = "worker"
	ComponentSharedInstance = "shared-instance"
	// ServiceKeyLabel/LastUsedAtAnnotation are the shared-instance idle-GC
	// substrate (docs/adr/0034) — the provisioner has no Postgres column to
	// put a last-active timestamp in, so reconcile.Loop compares this
	// annotation against SharedInstanceIdleTimeoutMs instead.
	ServiceKeyLabel      = "agent-fleet.dev/service-key"
	LastUsedAtAnnotation = "agent-fleet.dev/last-used-at"
)

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

// PreviewURLFor is a per-task subdomain serving the app at the ROOT path —
// nothing is stripped, so an app emitting root-absolute URLs (/assets/...,
// /api/..., client-side router links) resolves correctly. The previous form,
// https://<host>/<taskID>/app/, relied on a stripPrefix middleware and gave
// the app no way to learn its public base path, so those apps 404'd even when
// the pod was healthy (docs/adr/0038).
//
// host is the wildcard base domain (E2E_HOST, e.g. e2e.bnei.dev); shortID is
// already DNS-safe (dashes stripped, truncated to 20), so it drops straight in
// as a label. Covered by the single *.e2e.bnei.dev cert — see PreviewDomainFor.
func PreviewURLFor(host, taskID string) string {
	return fmt.Sprintf("https://%s.%s/", shortID(taskID), host)
}

// PreviewHostFor is the bare hostname behind PreviewURLFor, for Host() rules.
func PreviewHostFor(host, taskID string) string {
	return fmt.Sprintf("%s.%s", shortID(taskID), host)
}

// PreviewDomainFor is the wildcard every task's IngressRoute asks for. Every
// task requesting the SAME wildcard is the point: ACME orders it once and
// reuses it forever. Left implicit, Traefik would derive the concrete per-task
// hostname from the Host() rule and order a cert per session — Let's Encrypt
// allows 50 per registered domain per 7 days, shared with every other
// bnei.dev host, so that would exhaust issuance cluster-wide (docs/adr/0038).
func PreviewDomainFor(host string) string {
	return "*." + host
}

func Labels(taskID string) map[string]string {
	return map[string]string{
		ComponentLabel: ComponentE2eRun,
		TaskIDLabel:    taskID,
		"app":          "e2e-" + shortID(taskID),
	}
}

// SharedInstanceName is deterministic per (repo, serviceKey) — no
// task/randomness involved, since a shared instance's whole point is being
// found again on a later request, not created fresh each time
// (docs/adr/0034). repo/serviceKey are already short slugs (repo names and
// catalog keys), no truncation needed unlike task-ID-derived names.
func SharedInstanceName(repo, serviceKey string) string {
	return "svc-" + repo + "-" + serviceKey
}

func SharedInstanceAdminSecretName(repo, serviceKey string) string {
	return SharedInstanceName(repo, serviceKey) + "-admin"
}

func SharedInstanceLabels(repo, serviceKey string) map[string]string {
	return map[string]string{
		ComponentLabel:  ComponentSharedInstance,
		RepoLabel:       repo,
		ServiceKeyLabel: serviceKey,
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
