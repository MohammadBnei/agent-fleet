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

// TestCreateWorkerPod_ResourcesWithinLimitRange pins two files that must move
// together and live in different languages, directories and deploy paths: the
// worker container's limits here, and limitRange.max in k8s/core.yaml.
//
// A LimitRange is namespace-wide, not release-scoped — core's release creates
// it, and it governs the pods the *provisioner* creates. A container limit
// above max is rejected at admission, so the pod is never created rather than
// merely throttled: no crash, no event on anything you are watching, just a
// session that never gets a pod.
//
// This test existed for the e2e sandbox and was deleted with it
// (docs/adr/0048 §6), which left the ceiling unpinned at exactly the moment
// the worker inherited the sandbox's job. Restored against the container that
// runs builds now.
//
// Like core/internal/buildguard, this reads a file OUTSIDE the module, which
// Go's test cache cannot see — a cached PASS survives edits to the very file
// being guarded. Run with -count=1.
func TestCreateWorkerPod_ResourcesWithinLimitRange(t *testing.T) {
	raw := repoFile(t, "k8s/core.yaml")

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
		t.Fatal("k8s/core.yaml must set limitRange explicitly — the chart default caps cpu at \"2\", below the worker's limit")
	}

	for _, tc := range []struct{ name, container, max string }{
		{"cpu", workerMaxCPU, values.LimitRange.Max["cpu"]},
		{"memory", workerMaxMemory, values.LimitRange.Max["memory"]},
	} {
		containerLimit := resource.MustParse(tc.container)
		nsMax := resource.MustParse(tc.max)
		if containerLimit.Cmp(nsMax) > 0 {
			t.Errorf("worker %s limit %s exceeds limitRange.max %s in k8s/core.yaml — the pod would be rejected at admission and never created; raise the max or lower the limit",
				tc.name, tc.container, tc.max)
		}
	}

	// ADR-0084 §9: a definition's resources reach the pod as Guaranteed QoS.
	// Worker, sidecar and clone all have requests == limits, the worker takes
	// the definition's numbers, the sidecar 256Mi/100m, and every limit fits
	// the LimitRange max (the largest definition load accepts is that max).
	for _, d := range []struct{ cpu, mem string }{
		{"500m", "1536Mi"},
		{values.LimitRange.Max["cpu"], values.LimitRange.Max["memory"]},
	} {
		c := newTestClient()
		ctx := context.Background()
		if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
			SessionID: "task-g", Repo: "r", LeaseID: "l",
			Definition: &DefinitionProfile{Name: "d", ProxyURL: "http://p", DeadlineSeconds: 60, CPU: d.cpu, Memory: d.mem},
		}); err != nil {
			t.Fatalf("CreateWorkerPod: %v", err)
		}
		job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-g"), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		ps := job.Spec.Template.Spec
		all := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
		want := map[string][2]string{"worker": {d.cpu, d.mem}, "sidecar": {"100m", "256Mi"}}
		for _, ct := range all {
			if !equalLists(ct.Resources.Requests, ct.Resources.Limits) {
				t.Errorf("%s: requests %v != limits %v, pod is not Guaranteed", ct.Name, ct.Resources.Requests, ct.Resources.Limits)
			}
			if w, ok := want[ct.Name]; ok {
				if got := ct.Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse(w[0])) != 0 {
					t.Errorf("%s cpu = %s, want %s", ct.Name, got.String(), w[0])
				}
				if got := ct.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse(w[1])) != 0 {
					t.Errorf("%s memory = %s, want %s", ct.Name, got.String(), w[1])
				}
			}
			for name, max := range map[corev1.ResourceName]string{corev1.ResourceCPU: values.LimitRange.Max["cpu"], corev1.ResourceMemory: values.LimitRange.Max["memory"]} {
				if l := ct.Resources.Limits[name]; l.Cmp(resource.MustParse(max)) > 0 {
					t.Errorf("%s %s limit %s over limitRange.max %s", ct.Name, name, l.String(), max)
				}
			}
		}
	}
}

func equalLists(a, b corev1.ResourceList) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for k, v := range a {
		w := b[k]
		if v.Cmp(w) != 0 {
			return false
		}
	}
	return true
}

// TestCreateWorkerPod_WorkerResources asserts the shape actually reaching the
// API server, so the constants above cannot drift from what CreateWorkerPod
// builds.
//
// Both halves are load-bearing and neither is cosmetic:
//
//   - The memory limit. cgroup v2 sets memory.oom.group on a container scope,
//     so a build that crosses the limit does not get killed on its own — the
//     kernel SIGKILLs every process in the container, the agent and the
//     worker's own PID 1 included. A revert to 2Gi does not produce a failed
//     Bash call, it produces a session that vanishes mid-sentence with no
//     error line anywhere (live, 2026-08-17: an ordinary `bun run build`).
//   - The CPU *request*, which sets the CFS weight. The 250m this container
//     carried while builds lived in the sandbox meant compiling with a
//     quarter core on any busy node — the same finding bc5da8f recorded for
//     the sandbox before docs/adr/0048 §6 moved the builds here without the
//     sizing.
func TestCreateWorkerPod_WorkerResources(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
		SessionID: "task-res", Repo: "dream-analyst", LeaseID: "lease-1",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-res"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var worker *corev1.Container
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == "worker" {
			worker = &job.Spec.Template.Spec.Containers[i]
		}
	}
	if worker == nil {
		t.Fatal("no worker container")
	}
	for _, tc := range []struct {
		name string
		got  resource.Quantity
		want string
	}{
		{"cpu request", worker.Resources.Requests[corev1.ResourceCPU], "100m"},
		{"memory request", worker.Resources.Requests[corev1.ResourceMemory], "1Gi"},
		{"cpu limit", worker.Resources.Limits[corev1.ResourceCPU], workerMaxCPU},
		{"memory limit", worker.Resources.Limits[corev1.ResourceMemory], workerMaxMemory},
	} {
		want := resource.MustParse(tc.want)
		if tc.got.Cmp(want) != 0 {
			t.Errorf("worker %s = %s, want %s", tc.name, tc.got.String(), tc.want)
		}
	}
}

// repoFile reads rel from the repo root, outside this module. Go's test cache
// cannot see such a file, so tests using it need -count=1.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	// provisioner/internal/k8s -> provisioner/internal -> provisioner -> repo root
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("read %s — this guard is worthless if it silently skips: %v", rel, err)
	}
	return raw
}
