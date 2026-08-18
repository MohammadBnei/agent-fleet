package k8s

import (
	"fmt"
	"strings"
)

const (
	// AppPort, CodeServerPort, PlaywrightPort, ExecPort and SSHPort described
	// the five listeners of the e2e pod. There is no e2e pod (docs/adr/0048
	// §6), and the port a session serves on is now whatever the agent chose
	// and passed to expose() — a parameter, not a constant.

	// SidecarMCPPort is agent-facing (local MCP server, the Agent SDK's
	// mcpServers config), SidecarAPIPort is wrapper-facing (plain local
	// HTTP/JSON, docs/adr/0020 point 5's two local surfaces).
	SidecarMCPPort = 9090
	SidecarAPIPort = 9091
	ComponentLabel = "agent-fleet.dev/component"
	TaskIDLabel    = "agent-fleet.dev/task-id"
	// RepoLabel lets the provisioner recover which repo a session's pod
	// belongs to from the pod itself — it holds no DB credentials to look it
	// up any other way (docs/adr/0020 point 1), so Kubernetes has to carry it.
	RepoLabel               = "agent-fleet.dev/repo"
	ComponentWorker         = "worker"
	ComponentSharedInstance = "shared-instance"
	// ServiceKeyLabel/LastUsedAtAnnotation are the shared-instance idle-GC
	// substrate (docs/adr/0034) — the provisioner has no Postgres column to
	// put a last-active timestamp in, so reconcile.Loop compares this
	// annotation against SharedInstanceIdleTimeoutMs instead.
	ServiceKeyLabel      = "agent-fleet.dev/service-key"
	LastUsedAtAnnotation = "agent-fleet.dev/last-used-at"
)

// ResourceName named the e2e pod and its Service/route. Gone with them
// (docs/adr/0048 §6); the two names left are WorkerResourceName for the
// session's Job and ExposeResourceName for what expose() creates.

// WorkerResourceName is a session's Job (and therefore its pod).
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

// The ServiceEndpoint roster (EndpointsFor/EndpointsJSON, the endpoint-name
// and protocol constants, and the URL builders for the sandbox's Playwright
// and exec listeners) used to live here. It was docs/adr/0045's whole
// service-discovery mechanism — addresses derived from names and shipped on
// responses core already sent, so a sidecar could dial its task's sandbox
// without a lookup RPC.
//
// It is gone with the sandbox (docs/adr/0048 §6). Nothing is dialled by
// address any more: the agent's tools are on its own pod's localhost, and its
// app is in the same container namespace it is.
//
// PreviewHostFor survives because expose() still needs a hostname. The rest of
// the URL builders do not: PreviewURLFor and CodeServerURLFor described an app
// and an IDE the fleet used to start, and it starts neither.

// PreviewHostFor is a per-session subdomain, serving at the ROOT path.
//
// Nothing is stripped, which is the point of docs/adr/0038: an app emitting
// root-absolute URLs (/assets/..., /api/..., client-side router links) resolves
// correctly. The previous form, https://<host>/<taskID>/app/, relied on a
// stripPrefix middleware and gave the app no way to learn its public base
// path, so those apps 404'd even when the pod was perfectly healthy.
//
// host is the BASE domain (E2E_HOST, now bnei.dev — it used to be the deeper
// e2e.bnei.dev). shortID is already DNS-safe (dashes stripped, truncated to
// 20), and "-e2e" is appended to it rather than kept as its own DNS label.
//
// That single dash is load-bearing. Cloudflare's free Universal SSL covers the
// apex plus exactly ONE wildcard level, so *.bnei.dev secures
// abc123-e2e.bnei.dev but nothing secures abc123.e2e.bnei.dev — a proxied
// request to the latter gets no certificate at all and the TLS handshake
// fails before HTTP happens. That was confirmed in production on
// 2026-08-18 against dev.api.voconsteroid.com, which failed exactly this way
// while its one-label-shallower sibling served fine.
//
// Keeping previews first-level also means they are covered by the same
// *.bnei.dev origin certificate as every other host, so no preview ever
// triggers its own ACME order.
func PreviewHostFor(host, taskID string) string {
	return fmt.Sprintf("%s-e2e.%s", shortID(taskID), host)
}

// PreviewDomainFor is the wildcard every session's IngressRoute asks for. Every
// task requesting the SAME wildcard is the point: ACME orders it once and
// reuses it forever. Left implicit, Traefik would derive the concrete per-task
// hostname from the Host() rule and order a cert per session — Let's Encrypt
// allows 50 per registered domain per 7 days, shared with every other
// bnei.dev host, so that would exhaust issuance cluster-wide (docs/adr/0038).
//
// Unchanged by the first-level move: with E2E_HOST now the base domain, this
// returns *.bnei.dev, which is the same wildcard Traefik's default TLSStore
// holds. Previews therefore share the cluster-wide origin certificate rather
// than owning one of their own.
func PreviewDomainFor(host string) string {
	return "*." + host
}

// Labels was the e2e pod's label set, and went with it. WorkerLabels below is
// the one a session's pod actually carries.

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
		// The standard fleet-wide selector. It was originally added because
		// the e2e pod's NetworkPolicy matched on it; that policy is gone, but
		// the label is the conventional one every other object here carries.
		"app.kubernetes.io/part-of": "agent-fleet",
	}
}

// WorkerSelectorLabels is the subset of WorkerLabels that identifies a
// session's pod and nothing else — what a Service selector needs.
//
// Deliberately NOT the whole label set: `repo` is on the pod too, and a
// selector including it would still work today but would silently start
// matching a second pod the moment anything else in this namespace carried the
// same repo label. A selector is a live query, not a description.
func WorkerSelectorLabels(taskID string) map[string]string {
	return map[string]string{
		ComponentLabel: ComponentWorker,
		TaskIDLabel:    taskID,
	}
}

// ExposeResourceName names the Service and IngressRoute backing expose().
//
// Distinct from WorkerResourceName so that unexposing can never delete the
// session's Job by name collision, and so the objects are obviously not the
// pod's own when someone is reading `kubectl get all`.
func ExposeResourceName(sessionID string) string {
	return "expose-" + shortID(sessionID)
}
