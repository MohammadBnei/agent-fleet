package k8s

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// TestCreatePod_ResourcesWithinLimitRange pins two files that must move
// together and live in different languages, directories and deploy paths:
// the e2e-runner's container limits here, and limitRange.max in
// k8s/core.yaml.
//
// A LimitRange is namespace-wide, not release-scoped — core's release
// creates it, and it governs the pods the *provisioner* creates. A
// container limit above max is rejected at admission, so the pod is never
// created rather than merely throttled. The e2e pod's CPU limit sat exactly
// on the chart's default ceiling of "2" for its whole life, so the first
// person to raise it for a slow build would have hit this.
//
// Like core/internal/buildguard, this reads a file OUTSIDE the module,
// which Go's test cache cannot see — a cached PASS survives edits to the
// very file being guarded. Run with -count=1.
func TestCreatePod_ResourcesWithinLimitRange(t *testing.T) {
	// provisioner/internal/k8s -> provisioner/internal -> provisioner -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "k8s", "core.yaml"))
	if err != nil {
		t.Fatalf("read k8s/core.yaml — this guard is worthless if it silently skips: %v", err)
	}

	var values struct {
		LimitRange struct {
			Enabled bool              `json:"enabled"`
			Max     map[string]string `json:"max"`
		} `json:"limitRange"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse k8s/core.yaml: %v", err)
	}
	if !values.LimitRange.Enabled {
		t.Fatal("k8s/core.yaml must set limitRange explicitly — the chart default caps cpu at \"2\", below the e2e-runner's limit")
	}

	for _, tc := range []struct{ name, container, max string }{
		{"cpu", e2eMaxCPU, values.LimitRange.Max["cpu"]},
		{"memory", e2eMaxMemory, values.LimitRange.Max["memory"]},
	} {
		containerLimit := resource.MustParse(tc.container)
		nsMax := resource.MustParse(tc.max)
		if containerLimit.Cmp(nsMax) > 0 {
			t.Errorf("e2e-runner %s limit %s exceeds limitRange.max %s in k8s/core.yaml — the pod would be rejected at admission and never created; raise the max or lower the limit",
				tc.name, tc.container, tc.max)
		}
	}
}

// TestCreatePod_E2eResources asserts the shape actually reaching the API
// server, so the constants above can't drift from what CreatePod builds.
// Requests matter more than the limit here: they set the CFS weight, and
// the old 250m meant a sandbox got a quarter core to install dependencies
// with whenever the node was busy.
func TestCreatePod_E2eResources(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreatePod(ctx, TaskRef{ID: "task-1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "e2e-runner" {
			runner = &pod.Spec.Containers[i]
		}
	}
	if runner == nil {
		t.Fatal("no e2e-runner container")
	}
	for _, tc := range []struct {
		name string
		got  resource.Quantity
		want string
	}{
		{"cpu request", runner.Resources.Requests[corev1.ResourceCPU], "1000m"},
		{"memory request", runner.Resources.Requests[corev1.ResourceMemory], "1Gi"},
		{"cpu limit", runner.Resources.Limits[corev1.ResourceCPU], e2eMaxCPU},
		{"memory limit", runner.Resources.Limits[corev1.ResourceMemory], e2eMaxMemory},
	} {
		want := resource.MustParse(tc.want)
		if tc.got.Cmp(want) != 0 {
			t.Errorf("e2e-runner %s = %s, want %s", tc.name, tc.got.String(), tc.want)
		}
	}
}
