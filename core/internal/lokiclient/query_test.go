package lokiclient

import (
	"testing"
)

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
			want: `{namespace="agent-fleet", app=~"worker-.*"} | json | level="error"`,
		},
		{
			name: "core without filters",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "core",
			},
			want: `{namespace="agent-fleet", app="core"} | json`,
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
			name: "with task ID filter",
			req: QueryRequest{
				Namespace: "agent-fleet",
				Component: "worker",
				TaskID:    "abc-123",
				Level:     "warn",
			},
			want: `{namespace="agent-fleet", app=~"worker-.*"} | task_id="abc-123" | json | level="warn"`,
		},
		{
			name: "default namespace",
			req: QueryRequest{
				Component: "provisioner",
			},
			want: `{namespace="agent-fleet", app="provisioner"} | json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLogQL(tt.req)
			if got != tt.want {
				t.Errorf("buildLogQL() = %v, want %v", got, tt.want)
			}
		})
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
			want:      `{namespace="agent-fleet", app=~"worker-.*"}`,
		},
		{
			name:      "sidecar",
			namespace: "agent-fleet",
			component: "sidecar",
			want:      `{namespace="agent-fleet", app=~"worker-.*"}`,
		},
		{
			name:      "core",
			namespace: "agent-fleet",
			component: "core",
			want:      `{namespace="agent-fleet", app="core"}`,
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
				t.Errorf("buildSelector() = %v, want %v", got, tt.want)
			}
		})
	}
}
