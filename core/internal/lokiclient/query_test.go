package lokiclient

import (
	"strings"
	"testing"
)

// Every expected string below was verified against the live Loki label set
// (`/loki/api/v1/series` over namespace="agent-fleet") rather than derived from
// the implementation. That distinction matters: the previous version of these
// tests asserted the selectors the code happened to build, all four of which
// matched zero streams, so the suite stayed green while the log viewer was
// entirely non-functional. If one of these needs changing, re-check Loki first
// — do not update the expectation to match new code.
//
// Ground truth at time of writing:
//	job = "agent-fleet/worker-<shortId>-<suffix>"   (namespace/pod, NOT worker-*)
//	job = "agent-fleet/provisioner-<suffix>"
//	app = "agent-fleet-core"                        (NOT "core")
//	app = ""                                        for the provisioner
//	app = "e2e-<shortId>"                           for e2e pods

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
			want: `{namespace="agent-fleet", job=~".*/worker-.*"} | json | level="error"`,
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
			// The regression that made every task-scoped query empty: the
			// filter must come AFTER | json (it reads a parsed field, not a
			// stream label) and must use the camelCase name the services emit.
			name: "with task ID filter",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "worker",
				TaskID:    "abc-123",
				Level:     "warn",
			},
			want: `{namespace="agent-fleet", job=~".*/worker-.*"} | json | taskId="abc-123" | level="warn"`,
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

// A field filter placed before `| json` is a label matcher against a stream
// label that does not exist, so it matches nothing and the query silently
// returns zero lines — no error, no warning, just an empty log pane. Guarding
// the ordering directly means a future refactor cannot reintroduce it.
func TestBuildLogQL_TaskFilterComesAfterJSONParse(t *testing.T) {
	got := buildLogQL(QueryRequest{Component: "worker", TaskID: "abc-123"})
	jsonAt := strings.Index(got, "| json")
	taskAt := strings.Index(got, `| taskId=`)
	if jsonAt < 0 || taskAt < 0 {
		t.Fatalf("expected both a json parse and a taskId filter, got %q", got)
	}
	if taskAt < jsonAt {
		t.Errorf("taskId filter must come after | json, got %q", got)
	}
	if strings.Contains(got, "task_id") {
		t.Errorf("filter must use the emitted camelCase field taskId, got %q", got)
	}
}

// LogQL fully anchors regex label matchers, so a pattern must account for the
// "<namespace>/" prefix Alloy puts on job. `worker-.*` cannot match
// "agent-fleet/worker-abc" — this is the whole reason worker logs were empty.
func TestBuildSelector_JobPatternsTolerateNamespacePrefix(t *testing.T) {
	for _, component := range []string{"worker", "sidecar", "provisioner"} {
		sel := buildSelector("agent-fleet", component, "")
		if !strings.Contains(sel, `job=~".*/`) {
			t.Errorf("%s selector must match the namespace/pod job format, got %q", component, sel)
		}
	}
}

func TestBuildSelector(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		component string
		appName   string
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
			want:      `{namespace="agent-fleet", app=~"e2e-.*"}`,
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
			got := buildSelector(tt.namespace, tt.component, tt.appName)
			if got != tt.want {
				t.Errorf("buildSelector()\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}
