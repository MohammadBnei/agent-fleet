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

// ADR-0086 §5: the sidecar mounts only the repo its set may write; the worker sees only memory, read-only, never quarantine.
func TestMemoryMounts(t *testing.T) {
	for _, c := range []struct {
		set, sidecar string
	}{
		{"BC", "/memory/agent-kai-memory"}, {"AB", "/memory/agent-kai-quarantine"}, {"AC", "/memory/agent-kai-quarantine"},
		{"C", "/memory/agent-kai-memory"}, {"B", ""}, {"A", "/memory/agent-kai-quarantine"},
	} {
		ps := memJob(t, c.set)
		var sc corev1.Container
		for _, ic := range ps.InitContainers {
			if ic.Name == "sidecar" {
				sc = ic
			}
		}
		all, ro := mounts(sc)
		if (c.sidecar == "" && len(all) != 0) || (c.sidecar != "" && (len(all) != 1 || all[0] != c.sidecar)) || len(ro) != 0 {
			t.Errorf("%s: sidecar mounts %v ro %v, want only %q writable", c.set, all, ro, c.sidecar)
		}
		all, ro = mounts(ps.Containers[0])
		if strings.Contains(c.set, "B") {
			if len(all) != 1 || all[0] != "/memory/agent-kai-memory" || len(ro) != 1 {
				t.Errorf("%s: worker mounts %v ro %v, want memory read-only only", c.set, all, ro)
			}
		} else if len(all) != 0 {
			t.Errorf("%s: worker without B mounted memory: %v", c.set, all)
		}
	}
}

func TestMemoryNeedsSidecarAndValidSet(t *testing.T) {
	ps := &corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}
	if err := applyMemory(ps, MemoryProfile{Agent: "kai", Set: "BC"}); err == nil {
		t.Error("pod without sidecar accepted")
	}
	ps = &corev1.PodSpec{InitContainers: []corev1.Container{{Name: "sidecar"}}, Containers: []corev1.Container{{Name: "worker"}}}
	for _, set := range []string{"", "ab", "{A,B}", "CB"} {
		if err := applyMemory(ps, MemoryProfile{Agent: "kai", Set: set}); err == nil {
			t.Errorf("set %q accepted", set)
		}
	}
}

func TestMemoryBadName(t *testing.T) {
	c := newTestClient()
	err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "t2", Repo: "r", LeaseID: "l", Memory: &MemoryProfile{Agent: "../x", Set: "BC"}})
	if err == nil {
		t.Error("bad agent name accepted")
	}
}
