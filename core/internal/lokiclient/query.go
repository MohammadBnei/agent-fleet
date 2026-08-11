package lokiclient

import (
	"fmt"
	"strings"
)

// buildLogQL constructs a LogQL query from the request filters.
func buildLogQL(req QueryRequest) string {
	var parts []string

	// Base selector: namespace + component/app + task. The task filter lives
	// INSIDE the selector, as a stream label — Alloy promotes agent-fleet's
	// agent-fleet.dev/task-id pod label to task_id (infra-bootstrap ADR-0027's
	// pipeline). It was previously a `| task_id=` line filter placed before
	// `| json`, which made it a matcher against a stream label that did not
	// exist, so it silently dropped every line and task-scoped queries always
	// came back empty.
	//
	// A stream label is also the only thing that works for e2e-preview pods:
	// their output is the target app's (vite, code-server, Chromium), not the
	// fleet's JSON, so any `| json`-based filter discards all of it.
	selector := buildSelector(req.Namespace, req.Component, req.AppName, req.TaskID)
	parts = append(parts, selector)

	// JSON parsing. Must precede any field filter below; only meaningful for
	// the fleet's own Go/TS services, and harmless for e2e's plain-text lines
	// (unparseable lines pass through with no extracted labels).
	parts = append(parts, `| json`)

	// Level filter. Uppercased because LogQL string equality is case-sensitive
	// and every fleet logger emits uppercase — Go's slog writes "INFO", and the
	// worker's logger calls .toUpperCase() for exactly that parity. The
	// QueryRequest contract stays lowercase ("info"), which is what the API
	// documents and the dashboard sends, so the normalisation belongs here.
	// Without it `| level="info"` matched zero lines while `| level="INFO"`
	// matched everything.
	if req.Level != "" {
		parts = append(parts, fmt.Sprintf(`| level="%s"`, strings.ToUpper(req.Level)))
	}

	return strings.Join(parts, " ")
}

// buildSelector constructs the log stream selector part of LogQL.
func buildSelector(namespace, component, appName, taskID string) string {
	if namespace == "" {
		namespace = "agent-fleet"
	}

	// Start with namespace
	selector := fmt.Sprintf(`{namespace="%s"`, namespace)

	// task_id is a real stream label (Alloy promotes the pod label), so this
	// narrows at the index rather than after parsing — and works for every
	// component, including e2e pods whose lines are not JSON.
	if taskID != "" {
		selector += fmt.Sprintf(`, task_id="%s"`, taskID)
	}

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
		// e2e pods set a plain `app` pod label, but Alloy sources the `app`
		// stream label from app.kubernetes.io/instance — which these pods do
		// not set. So they reach Loki with no app label at all and the old
		// `app=~"e2e-.*"` matched nothing. job is the handle, as above.
		selector += `, job=~".*/e2e-.*"`
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
