package lokiclient

import (
	"strings"
	"testing"
)

// Every expected string below was verified against the live Loki label set
// (`/loki/api/v1/series` over namespace="agent-fleet") rather than derived from
// the implementation. That distinction matters: the previous version of these
// tests asserted the selectors the code happened to build — all five of which
// matched zero streams — so the suite stayed green while the log viewer was
// entirely non-functional. If one of these needs changing, re-check Loki first
// — do not update the expectation to match new code.
//
// Ground truth at time of writing:
//	job     = "agent-fleet/worker-<shortId>-<suffix>"  (namespace/pod, NOT worker-*)
//	job     = "agent-fleet/provisioner-<suffix>"
//	job     = "agent-fleet/e2e-<shortId>"
//	app     = "agent-fleet-core"                       (NOT "core")
//	app     = ""                                       for the provisioner
//	app     = ""                                       for e2e pods too — Alloy
//	          sources `app` from app.kubernetes.io/instance, which e2e pods do
//	          not set, so their plain `app` pod label never reaches Loki
//	task_id = the agent-fleet.dev/task-id pod label, promoted by Alloy

func TestBuildLogQL(t *testing.T) {
	tests := []struct {
		name string
		req  QueryRequest
		want string
	}{
		{
			name: "worker with level filter",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "worker",
				Level:     "error",
			},
			want: `{namespace="agent-fleet", job=~".*/worker-.*"} | json | level="ERROR"`,
		},
		{
			name: "core without filters",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "core",
			},
			want: `{namespace="agent-fleet", app="agent-fleet-core"} | json`,
		},
		{
			name: "deployed app",
			req: QueryRequest{
				Namespace: "default",
				Component: "app",
				AppName:   "dream-analyst",
			},
			want: `{namespace="default", app="dream-analyst"} | json`,
		},
		{
			// The regression that made every task-scoped query empty. task_id
			// is a real stream label (Alloy promotes the pod label), so it
			// belongs in the selector — not as a line filter, and certainly
			// not one placed before | json, which is what shipped.
			name: "with task ID filter",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "worker",
				TaskID:    "abc-123",
				Level:     "warn",
			},
			want: `{namespace="agent-fleet", task_id="abc-123", job=~".*/worker-.*"} | json | level="WARN"`,
		},
		{
			// e2e lines are the target app's own output (vite, code-server,
			// Chromium), not fleet JSON — so task scoping has to survive
			// without any parsed field to filter on.
			name: "e2e scoped to a task",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "e2e",
				TaskID:    "abc-123",
			},
			want: `{namespace="agent-fleet", task_id="abc-123", job=~".*/e2e-.*"} | json`,
		},
		{
			name: "default namespace",
			req: QueryRequest{
				Component: "provisioner",
			},
			want: `{namespace="agent-fleet", job=~".*/provisioner-.*"} | json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLogQL(tt.req)
			if got != tt.want {
				t.Errorf("buildLogQL()\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}

// The task filter must stay a stream label inside the selector. As a line
// filter it silently matches nothing when placed before `| json` (which is
// what shipped), and even placed correctly it would discard every e2e line,
// since those are not JSON. Either way the symptom is an empty log pane with
// no error — so guard the shape directly.
func TestBuildLogQL_TaskFilterIsAStreamLabel(t *testing.T) {
	for _, component := range []string{"worker", "sidecar", "core", "provisioner", "e2e"} {
		got := buildLogQL(QueryRequest{Component: component, TaskID: "abc-123"})
		selectorEnd := strings.Index(got, "}")
		taskAt := strings.Index(got, `task_id="abc-123"`)
		if taskAt < 0 {
			t.Errorf("%s: task filter missing entirely, got %q", component, got)
			continue
		}
		if taskAt > selectorEnd {
			t.Errorf("%s: task filter must be inside the stream selector, got %q", component, got)
		}
		if strings.Contains(got, `| taskId=`) || strings.Contains(got, `| task_id=`) {
			t.Errorf("%s: task filter must not be a line filter, got %q", component, got)
		}
	}
}

// LogQL fully anchors regex label matchers, so a pattern must account for the
// "<namespace>/" prefix Alloy puts on job. `worker-.*` cannot match
// "agent-fleet/worker-abc" — this is the whole reason worker logs were empty.
func TestBuildSelector_JobPatternsTolerateNamespacePrefix(t *testing.T) {
	for _, component := range []string{"worker", "sidecar", "provisioner"} {
		sel := buildSelector("agent-fleet", component, "", "")
		if !strings.Contains(sel, `job=~".*/`) {
			t.Errorf("%s selector must match the namespace/pod job format, got %q", component, sel)
		}
	}
}

// LogQL string equality is case-sensitive and every fleet logger emits
// uppercase (slog writes "INFO"; the worker calls .toUpperCase() to match).
// The request contract is lowercase, so `| level="info"` matched nothing while
// the logs were full of "INFO".
func TestBuildLogQL_LevelIsUppercased(t *testing.T) {
	for _, lvl := range []string{"info", "Warn", "ERROR"} {
		got := buildLogQL(QueryRequest{Component: "core", Level: lvl})
		want := `| level="` + strings.ToUpper(lvl) + `"`
		if !strings.Contains(got, want) {
			t.Errorf("level %q: expected %s in %q", lvl, want, got)
		}
	}
}

func TestBuildSelector(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		component string
		appName   string
		taskID    string
		want      string
	}{
		{
			name:      "worker",
			namespace: "agent-fleet",
			component: "worker",
			want:      `{namespace="agent-fleet", job=~".*/worker-.*"}`,
		},
		{
			// Same pod as the worker, so the same job label; the two are
			// separated by container downstream, not in the selector.
			name:      "sidecar",
			namespace: "agent-fleet",
			component: "sidecar",
			want:      `{namespace="agent-fleet", job=~".*/worker-.*"}`,
		},
		{
			name:      "core",
			namespace: "agent-fleet",
			component: "core",
			want:      `{namespace="agent-fleet", app="agent-fleet-core"}`,
		},
		{
			// The provisioner Deployment carries no app label at all, so the
			// job label is the only available handle.
			name:      "provisioner",
			namespace: "agent-fleet",
			component: "provisioner",
			want:      `{namespace="agent-fleet", job=~".*/provisioner-.*"}`,
		},
		{
			name:      "e2e",
			namespace: "agent-fleet",
			component: "e2e",
			want:      `{namespace="agent-fleet", job=~".*/e2e-.*"}`,
		},
		{
			name:      "deployed app",
			namespace: "default",
			component: "app",
			appName:   "vos-monolith",
			want:      `{namespace="default", app="vos-monolith"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSelector(tt.namespace, tt.component, tt.appName, tt.taskID)
			if got != tt.want {
				t.Errorf("buildSelector()\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}
