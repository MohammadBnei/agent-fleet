package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
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
	initContainers, env, vol, mount, err := buildIngredients([]string{"cluster-access"}, nil, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(initContainers))
	}
	if vol == nil || mount == nil {
		t.Fatal("expected a shared tools volume+mount when tool ingredients are present")
	}
	var path string
	for _, e := range env {
		switch e.Name {
		case "PATH":
			path = e.Value
		case "GOROOT":
			// GOROOT went with go-toolchain (docs/adr/0048 §6). Setting it
			// would point the image's own Go at a directory nothing stages.
			t.Error("GOROOT is back — the toolchain lives in repos.image now")
		}
	}
	if path == "" {
		t.Fatal("expected PATH env var")
	}
	if strings.Contains(path, "/opt/tools/go/bin") {
		t.Errorf("PATH = %q still carries /opt/tools/go/bin — it shadowed the worker image's own Go", path)
	}
	if !strings.HasPrefix(path, toolsMountPath+"/bin:") {
		t.Errorf("PATH = %q, want it to start with %s/bin:", path, toolsMountPath)
	}
}

// Every tool init container must pin ImagePullPolicy explicitly. Left to
// Kubernetes' default it is per-tag — IfNotPresent for a pinned tag, but
// Always for `:latest` — so a `:latest` entry in the catalog silently
// re-pulls from Docker Hub on every pod creation, delaying Running and
// spending pull quota per dispatch. Asserted over the whole catalog, not a
// fixed pair, so a tool added later is covered without touching this test.
func TestBuildIngredients_ToolInitContainersPinPullPolicy(t *testing.T) {
	keys := make([]string, 0, len(catalog.Tools))
	for key := range catalog.Tools {
		keys = append(keys, key)
	}
	initContainers, _, _, _, err := buildIngredients(keys, nil, ClusterAccess{})
	if err != nil {
		t.Fatalf("buildIngredients: %v", err)
	}
	if len(initContainers) != len(keys) {
		t.Fatalf("expected %d init containers, got %d", len(keys), len(initContainers))
	}
	for _, c := range initContainers {
		if c.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s (%s): ImagePullPolicy = %q, want %q — a `:latest` image would re-pull every dispatch",
				c.Name, c.Image, c.ImagePullPolicy, corev1.PullIfNotPresent)
		}
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

func TestCreateWorkerPod_WithPodScopedService(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	refs := []ServiceIngredientRef{{Key: "redis", ScopeMode: ScopeModePodScoped}}

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-svc", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: refs, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// clone + sidecar + redis = 3 init containers (docs/adr/0048 §5 added the
	// clone, which prepares the working tree inside the pod).
	if len(job.Spec.Template.Spec.InitContainers) != 3 {
		t.Fatalf("expected 3 init containers (clone + sidecar + redis), got %+v", job.Spec.Template.Spec.InitContainers)
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

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-extra", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: extraEnv}); err != nil {
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

// Least privilege: a normal repo session must never carry the executor's
// token. CreateWorkerPod passes ClusterAccess in unconditionally — it holds
// the provisioner's own config, not a per-session decision — so the ONLY
// thing keeping the token out of an ordinary pod is the cluster-access key
// being absent. That is what this asserts.
//
// It used to pass other tool keys here; those are gone (docs/adr/0048 §6),
// so the cases that remain are no ingredients at all and services-only.
func TestBuildIngredients_WithoutClusterAccessGetsNoClusterCredentials(t *testing.T) {
	cluster := ClusterAccess{ExecutorAddr: "should-not-appear:9090", AuthToken: "secret"}
	for _, tc := range []struct {
		name     string
		toolKeys []string
		services []ServiceIngredientRef
	}{
		{name: "no ingredients"},
		{name: "services only", services: []ServiceIngredientRef{{Key: "redis", ScopeMode: ScopeModePodScoped}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, env, _, _, err := buildIngredients(tc.toolKeys, tc.services, cluster)
			if err != nil {
				t.Fatalf("buildIngredients: %v", err)
			}
			for _, e := range env {
				if e.Name == "EXECUTOR_ADDR" || e.Name == "THOT_AUTH_TOKEN" {
					t.Errorf("%s leaked into a pod without cluster-access", e.Name)
				}
			}
		})
	}
}
