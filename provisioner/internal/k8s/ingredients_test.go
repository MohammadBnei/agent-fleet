package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildIngredients_UnknownToolKey(t *testing.T) {
	if _, _, _, _, err := buildIngredients([]string{"not-a-real-tool"}, nil, ClusterAccess{}); err == nil {
		t.Fatal("expected error for unknown tool key")
	}
}

func TestBuildIngredients_UnknownServiceKey(t *testing.T) {
	refs := []ServiceIngredientRef{{Key: "not-a-real-service", ScopeMode: ScopeModePodScoped}}
	if _, _, _, _, err := buildIngredients(nil, refs, ClusterAccess{}); err == nil {
		t.Fatal("expected error for unknown service key")
	}
}

func TestBuildIngredients_ToolsProduceInitContainersAndPath(t *testing.T) {
	initContainers, env, vol, mount, err := buildIngredients([]string{"go-toolchain", "buf"}, nil, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(initContainers))
	}
	if vol == nil || mount == nil {
		t.Fatal("expected a shared tools volume+mount when tool ingredients are present")
	}
	foundPath, foundGoroot := false, false
	for _, e := range env {
		if e.Name == "PATH" {
			foundPath = true
		}
		if e.Name == "GOROOT" {
			foundGoroot = true
		}
	}
	if !foundPath {
		t.Error("expected PATH env var")
	}
	if !foundGoroot {
		t.Error("expected GOROOT env var when go-toolchain is present")
	}
}

func TestBuildIngredients_NoToolsNoVolume(t *testing.T) {
	_, _, vol, mount, err := buildIngredients(nil, nil, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if vol != nil || mount != nil {
		t.Error("expected no tools volume/mount when no tool ingredients are requested")
	}
}

func TestBuildIngredients_PodScopedServiceProducesNativeSidecar(t *testing.T) {
	refs := []ServiceIngredientRef{{Key: "postgres", ScopeMode: ScopeModePodScoped}}
	initContainers, env, _, _, err := buildIngredients(nil, refs, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != 1 {
		t.Fatalf("expected 1 init container (postgres sidecar), got %d", len(initContainers))
	}
	sidecar := initContainers[0]
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("expected native sidecar (RestartPolicy: Always)")
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.Exec == nil {
		t.Error("expected an exec StartupProbe")
	}
	found := false
	for _, e := range env {
		if e.Name == "DATABASE_URL" {
			found = true
		}
	}
	if !found {
		t.Error("expected DATABASE_URL env var")
	}
}

func TestBuildIngredients_TaskScopedServiceProducesNoInitContainer(t *testing.T) {
	// task-scoped/repo-scoped are minted by the caller (grpcserver.go), not
	// materialized here — buildIngredients must not add any container for
	// them, only pod-scoped goes through this path.
	refs := []ServiceIngredientRef{{Key: "postgres", ScopeMode: ScopeModeTaskScoped}}
	initContainers, env, _, _, err := buildIngredients(nil, refs, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != 0 {
		t.Errorf("expected 0 init containers for task-scoped service, got %d", len(initContainers))
	}
	if len(env) != 0 {
		t.Errorf("expected 0 env vars for task-scoped service (caller supplies ExtraEnv), got %+v", env)
	}
}

func TestCreatePod_WithToolIngredients(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	task := TaskRef{ID: "abc-tools", Repo: "agent-fleet", StartCmd: "true", ToolKeys: []string{"golangci-lint"}}

	if err := c.CreatePod(ctx, task); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName(task.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "tool-golangci-lint" {
		t.Fatalf("expected 1 tool init container, got %+v", pod.Spec.InitContainers)
	}
	foundToolsVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == toolsVolumeName {
			foundToolsVol = true
		}
	}
	if !foundToolsVol {
		t.Error("expected a shared tools volume on the pod")
	}
}

func TestCreateWorkerPod_WithPodScopedService(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	refs := []ServiceIngredientRef{{Key: "redis", ScopeMode: ScopeModePodScoped}}

	if err := c.CreateWorkerPod(ctx, "task-svc", "dream-analyst", "lease-1", "/workspace/worktrees/task-svc", "", 0, nil, refs, nil); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// sidecar (existing) + redis (new) = 2 init containers.
	if len(job.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("expected 2 init containers (sidecar + redis), got %+v", job.Spec.Template.Spec.InitContainers)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	found := false
	for _, e := range worker.Env {
		if e.Name == "REDIS_URL" {
			found = true
		}
	}
	if !found {
		t.Error("expected REDIS_URL on the worker container's env")
	}
}

func TestCreateWorkerPod_WithExtraEnv(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	extraEnv := []corev1.EnvVar{{Name: "DATABASE_URL", Value: "postgresql://task_abc:pw@svc-dream-analyst-postgres:5432/task_abc"}}

	if err := c.CreateWorkerPod(ctx, "task-extra", "dream-analyst", "lease-1", "/workspace/worktrees/task-extra", "", 0, nil, nil, extraEnv); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-extra"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	found := false
	for _, e := range worker.Env {
		if e.Name == "DATABASE_URL" && e.Value == extraEnv[0].Value {
			found = true
		}
	}
	if !found {
		t.Error("expected already-minted DATABASE_URL to be passed through onto the worker container")
	}
}

// TestBuildIngredients_ClusterAccessInjectsShimEnv guards the failure mode
// that has already bitten this feature twice: the code ships, nothing sets
// the env, and the result is a silently missing capability rather than an
// error. The kubectl shim exits 2 with "EXECUTOR_ADDR unset" if this
// regresses — better than silence, but still only visible mid-task.
func TestBuildIngredients_ClusterAccessInjectsShimEnv(t *testing.T) {
	cluster := ClusterAccess{ExecutorAddr: "thot-executor.thot.svc.cluster.local:9090", AuthToken: "tok"}
	initContainers, env, _, _, err := buildIngredients([]string{"cluster-access"}, nil, cluster)
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != 1 {
		t.Fatalf("expected one init container staging the shim, got %d", len(initContainers))
	}

	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["EXECUTOR_ADDR"] != cluster.ExecutorAddr {
		t.Errorf("EXECUTOR_ADDR = %q, want %q — the shim cannot reach the executor without it", got["EXECUTOR_ADDR"], cluster.ExecutorAddr)
	}
	if got["THOT_AUTH_TOKEN"] != cluster.AuthToken {
		t.Errorf("THOT_AUTH_TOKEN = %q, want %q — the executor will reject the call", got["THOT_AUTH_TOKEN"], cluster.AuthToken)
	}
}

// Least privilege: a normal repo task must never carry the executor's
// token just because it happens to use other ingredients.
func TestBuildIngredients_OtherToolsGetNoClusterCredentials(t *testing.T) {
	_, env, _, _, err := buildIngredients([]string{"go-toolchain", "buf"}, nil,
		ClusterAccess{ExecutorAddr: "should-not-appear:9090", AuthToken: "secret"})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	for _, e := range env {
		if e.Name == "EXECUTOR_ADDR" || e.Name == "THOT_AUTH_TOKEN" {
			t.Errorf("%s leaked into a pod without cluster-access", e.Name)
		}
	}
}
