package lokiclient

import (
	"fmt"
	"strings"
)

// buildLogQL constructs a LogQL query from the request filters.
func buildLogQL(req QueryRequest) string {
	var parts []string

	// Base selector: namespace + component/app
	selector := buildSelector(req.Namespace, req.Component, req.AppName)
	parts = append(parts, selector)

	// JSON parsing (all fleet services output JSON logs) MUST come before any
	// field filter below. It previously came after the task filter, which made
	// that filter a label matcher against a stream label that does not exist —
	// so it silently matched nothing and every task-scoped query returned no
	// logs at all.
	parts = append(parts, `| json`)

	// taskId, not task_id: the field name is whatever the services emit, and
	// Go's slog and the worker's logger both write camelCase taskId. Every
	// component stamps it on every line (core/provisioner pass it explicitly;
	// worker/sidecar stamp it globally at startup), so one filter works fleet-wide.
	if req.TaskID != "" {
		parts = append(parts, fmt.Sprintf(`| taskId="%s"`, req.TaskID))
	}

	// Level filter
	if req.Level != "" {
		parts = append(parts, fmt.Sprintf(`| level="%s"`, req.Level))
	}

	return strings.Join(parts, " ")
}

// buildSelector constructs the log stream selector part of LogQL.
func buildSelector(namespace, component, appName string) string {
	if namespace == "" {
		namespace = "agent-fleet"
	}

	// Start with namespace
	selector := fmt.Sprintf(`{namespace="%s"`, namespace)

	// Every value below was verified against the live Loki label set. Alloy
	// emits job as "<namespace>/<pod>", and LogQL fully anchors regex label
	// matchers — so the previous `job=~"worker-.*"` could never match
	// "agent-fleet/worker-abc123" and returned zero streams for every worker
	// and sidecar query. Same story for app: core's label is
	// "agent-fleet-core", not "core", and the provisioner carries no app
	// label at all. All four selectors matched nothing.
	switch component {
	case "worker":
		// Worker and sidecar share a pod, so they share a job label; they are
		// separated by `container` downstream, not here.
		selector += `, job=~".*/worker-.*"`
	case "sidecar":
		selector += `, job=~".*/worker-.*"`
	case "core":
		selector += `, app="agent-fleet-core"`
	case "provisioner":
		// No app label on this Deployment — the job label is the only handle.
		selector += `, job=~".*/provisioner-.*"`
	case "e2e":
		// E2E pods do carry app=e2e-<shortId> (k8s.Labels), so this one was
		// already correct.
		selector += `, app=~"e2e-.*"`
	case "app":
		// Deployed app: use appName for app label
		if appName != "" {
			selector += fmt.Sprintf(`, app="%s"`, appName)
		}
	default:
		// No app filter - returns all pods in namespace
	}

	selector += `}`
	return selector
}
