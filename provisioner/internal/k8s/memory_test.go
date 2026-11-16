package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func memJob(t *testing.T, set string) corev1.PodSpec {
	t.Helper()
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "t1", Repo: "r", LeaseID: "l", Memory: &MemoryProfile{Agent: "kai", Set: set}}); err != nil {
		t.Fatal(err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("t1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return job.Spec.Template.Spec
}

func mounts(c corev1.Container) (all, ro []string) {
	for _, m := range c.VolumeMounts {
		if strings.HasPrefix(m.MountPath, memoryDir) {
			all = append(all, m.MountPath)
			if m.ReadOnly {
				ro = append(ro, m.MountPath)
			}
		}
	}
	return
}

// ADR-0086 §5: the sidecar writes both repos; the worker sees only memory, read-only, never quarantine.
func TestMemoryMounts(t *testing.T) {
	for _, set := range []string{"BC", "AB"} {
		ps := memJob(t, set)
		var sc corev1.Container
		for _, c := range ps.InitContainers {
			if c.Name == "sidecar" {
				sc = c
			}
		}
		if all, ro := mounts(sc); len(all) != 2 || len(ro) != 0 {
			t.Errorf("%s: sidecar mounts %v ro %v, want two writable", set, all, ro)
		}
		all, ro := mounts(ps.Containers[0])
		if len(all) != 1 || all[0] != "/memory/agent-kai-memory" || len(ro) != 1 {
			t.Errorf("%s: worker mounts %v ro %v, want memory read-only only", set, all, ro)
		}
	}
	// A set without B reads no memory at all.
	if all, _ := mounts(memJob(t, "AC").Containers[0]); len(all) != 0 {
		t.Errorf("worker without B mounted memory: %v", all)
	}
}

func TestMemoryBadName(t *testing.T) {
	c := newTestClient()
	err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "t2", Repo: "r", LeaseID: "l", Memory: &MemoryProfile{Agent: "../x", Set: "BC"}})
	if err == nil {
		t.Error("bad agent name accepted")
	}
}
