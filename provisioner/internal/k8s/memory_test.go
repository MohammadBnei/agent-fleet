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

func TestMemoryWorkerEnv(t *testing.T) {
	env := func(set string) map[string]string {
		m := map[string]string{}
		for _, e := range memJob(t, set).Containers[0].Env {
			m[e.Name] = e.Value
		}
		return m
	}
	m := env("BC")
	if m["FLEET_MEMORY_DIR"] != "/memory/agent-kai-memory" || m["GIT_CONFIG_KEY_0"] != "safe.directory" || m["GIT_CONFIG_VALUE_0"] != "/memory/agent-kai-memory" {
		t.Errorf("worker env = %v", m)
	}
	if env("C")["FLEET_MEMORY_DIR"] != "" {
		t.Error("a worker without B was told where memory is")
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

func standingSpec(id string) WorkerPodSpec {
	return WorkerPodSpec{SessionID: id, Repo: "r", LeaseID: "l", Memory: &MemoryProfile{Agent: "kai", Set: "BC"},
		Definition: &DefinitionProfile{Name: "kai", ProxyURL: "http://p", DeadlineSeconds: 60, CPU: "500m", Memory: "1536Mi", Guaranteed: true}}
}

// ADR-0084 §3: a standing character's volume is Longhorn RWO; an ordinary session keeps the session class.
func TestStandingPVCClass(t *testing.T) {
	c := newTestClient()
	c.SessionStorageClass, c.StandingStorageClass = "local-path", "longhorn"
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, standingSpec("s1")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "o1", Repo: "r", LeaseID: "l"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"s1": "longhorn", "o1": "local-path"} {
		pvc, err := c.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, SessionPVCName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := *pvc.Spec.StorageClassName; got != want || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Errorf("%s: class %q modes %v, want %q RWO", id, got, pvc.Spec.AccessModes, want)
		}
	}
}

// A standing character without a resources block (Guaranteed=false) still gets the Longhorn class.
func TestStandingPVCClassWithoutResources(t *testing.T) {
	c := newTestClient()
	c.SessionStorageClass, c.StandingStorageClass = "local-path", "longhorn"
	ctx := context.Background()
	spec := WorkerPodSpec{SessionID: "s2", Repo: "r", LeaseID: "l", Memory: &MemoryProfile{Agent: "kai", Set: "BC"}}
	if err := c.CreateWorkerPod(ctx, spec); err != nil {
		t.Fatal(err)
	}
	pvc, err := c.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, SessionPVCName("s2"), metav1.GetOptions{})
	if err != nil || *pvc.Spec.StorageClassName != "longhorn" {
		t.Fatalf("pvc %v, %v; want longhorn", pvc, err)
	}
}
