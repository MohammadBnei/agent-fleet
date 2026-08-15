package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// workerContainer pulls the worker out of a created Job, or fails the test.
func workerContainer(t *testing.T, c *Client, sessionID string) *corev1.Container {
	t.Helper()
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), WorkerResourceName(sessionID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == "worker" {
			return &job.Spec.Template.Spec.Containers[i]
		}
	}
	t.Fatal("no worker container")
	return nil
}

// fsGroup is what makes every other volume decision here work, and nothing else
// in this package would notice its absence.
//
// Kubelet creates a subPath directory root-owned 0755; every container in this
// pod runs as uid 1000. Without fsGroup the clone init container cannot write
// /workspace and no package manager can write /cache — on a real StorageClass.
// It survived this long only because kind's hostPath happens to be
// world-writable, the same accident that hid the "dubious ownership" bug until
// it ran against a real cluster. The fake clientset enforces no permissions
// either, so this asserts the field rather than the behaviour.
func TestCreateWorkerPod_SetsFSGroupSoTheNonRootUserCanWriteItsVolumes(t *testing.T) {
	c := newTestClient()
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil {
		t.Fatal("no pod fsGroup: kubelet leaves the subPath dirs root-owned 0755, so the uid-1000 " +
			"containers cannot write /workspace or /cache on any real StorageClass")
	}
	if *sc.FSGroup != 1000 {
		t.Errorf("fsGroup = %d, want 1000 (the `bun` user from worker/Dockerfile)", *sc.FSGroup)
	}
}

// The /cache mount is only worth having if something writes to it.
//
// It shipped as a real mount that no tool pointed at: bun and Go default to
// $HOME, which is the container's ephemeral layer, so the volume survived every
// warm holding nothing and each warm paid for a cold install anyway. Nothing
// about that is visible — the mount exists, the pod is healthy, and the only
// symptom is being slow.
func TestCreateWorkerPod_PackageManagerCachesPointAtTheCacheMount(t *testing.T) {
	c := newTestClient()
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	worker := workerContainer(t, c, "task-1")

	// Checked by name: a package manager missing from this list silently
	// reverts to the ephemeral filesystem.
	for _, name := range []string{
		"BUN_INSTALL_CACHE_DIR", "GOMODCACHE", "GOCACHE", "npm_config_cache",
		"npm_config_store_dir", "YARN_CACHE_FOLDER", "UV_CACHE_DIR",
		"PIP_CACHE_DIR", "CARGO_HOME", "XDG_CACHE_HOME",
	} {
		value, ok := findEnv(worker.Env, name)
		if !ok {
			t.Errorf("%s is not set — that tool caches onto the pod's ephemeral layer, and every warm reinstalls cold", name)
			continue
		}
		if !strings.HasPrefix(value, sessionCacheDir+"/") {
			t.Errorf("%s = %q, not under the %s mount — it will not survive a warm", name, value, sessionCacheDir)
		}
	}

	// ...and the mount every one of those paths depends on has to be there.
	var mounted bool
	for _, m := range worker.VolumeMounts {
		if m.MountPath == sessionCacheDir {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("nothing mounted at %s, so every path above writes to the container filesystem", sessionCacheDir)
	}
}

// Sessions are confined to nodes that can afford a hostPath working tree: the
// control-plane nodes have ~33 GiB allocatable, and filling one is a cluster
// incident rather than a session failure.
//
// The second half is the one that matters more. An empty selector must not
// constrain anything, or /kind-local — a single unlabelled node — stops
// scheduling sessions entirely, which looks like a hung fleet rather than a
// misconfiguration.
func TestCreateWorkerPod_AppliesTheSessionNodeSelector(t *testing.T) {
	ctx := context.Background()

	c := newTestClient()
	c.SessionNodeSelector = map[string]string{"agent-fleet.dev/session-node": "true"}
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got := job.Spec.Template.Spec.NodeSelector["agent-fleet.dev/session-node"]; got != "true" {
		t.Errorf("node selector not applied, got %v", job.Spec.Template.Spec.NodeSelector)
	}

	plain := newTestClient()
	if err := plain.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-2", Repo: "dream-analyst", LeaseID: "lease-2"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job2, err := plain.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job2.Spec.Template.Spec.NodeSelector) != 0 {
		t.Errorf("an unset selector produced %v — kind's unlabelled node would never schedule a session",
			job2.Spec.Template.Spec.NodeSelector)
	}
}

// SESSION_STORAGE_CLASS is the difference between 10 MB/s and 1069 MB/s
// (docs/adr/0048 §4), and it has already been dropped on the floor once: it was
// parsed and forwarded but omitted from the k8s.Images literal, so every
// session PVC silently took the cluster default.
//
// Unset must still mean "cluster default" rather than a hardcoded name —
// naming a class that does not exist in a given cluster leaves every session
// Pending forever with no error anyone sees.
func TestEnsureSessionPVC_HonoursTheStorageClass(t *testing.T) {
	ctx := context.Background()

	c := newTestClient()
	c.SessionStorageClass = "local-path"
	if err := c.EnsureSessionPVC(ctx, "task-1", "dream-analyst"); err != nil {
		t.Fatalf("EnsureSessionPVC: %v", err)
	}
	pvc, err := c.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, SessionPVCName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "local-path" {
		t.Errorf("storage class = %v, want local-path — the session lands on the network volume otherwise",
			pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes = %v, want [ReadWriteOnce]", got)
	}

	plain := newTestClient()
	if err := plain.EnsureSessionPVC(ctx, "task-2", "dream-analyst"); err != nil {
		t.Fatalf("EnsureSessionPVC: %v", err)
	}
	pvc2, err := plain.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, SessionPVCName("task-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if pvc2.Spec.StorageClassName != nil {
		t.Errorf("unset config pinned a class (%v) — a cluster without it leaves every session Pending",
			*pvc2.Spec.StorageClassName)
	}
}
