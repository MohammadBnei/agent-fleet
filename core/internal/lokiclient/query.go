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

	// Task ID filter (pod label)
	if req.TaskID != "" {
		// Note: This assumes task-id is a stream label. If it's in the log line JSON,
		// we'd need to filter after | json parsing instead.
		parts = append(parts, fmt.Sprintf(`| task_id="%s"`, req.TaskID))
	}

	// JSON parsing (all fleet services output JSON logs)
	parts = append(parts, `| json`)

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

	switch component {
	case "worker":
		// Worker pods: job label (auto-added by k8s Job controller) includes task ID
		selector += `, job=~"worker-.*"`
	case "sidecar":
		// Sidecar runs in same pod as worker, same job label
		selector += `, job=~"worker-.*"`
	case "core":
		selector += `, app="core"`
	case "provisioner":
		selector += `, app="provisioner"`
	case "e2e":
		// E2E pods: app label includes task ID
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
