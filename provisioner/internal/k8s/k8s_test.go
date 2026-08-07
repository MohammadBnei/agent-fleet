package k8s

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient() *Client {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		middlewareGVR:   "MiddlewareList",
		ingressRouteGVR: "IngressRouteList",
	}
	return &Client{
		Core:         fake.NewSimpleClientset(),
		Dynamic:      dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		Namespace:    "agent-fleet",
		RunnerImage:  "mohammaddocker/agent-fleet-e2e-runner:latest",
		WorkerImage:  "mohammaddocker/agent-fleet-worker:latest",
		SidecarImage: "mohammaddocker/agent-fleet-sidecar:latest",
		WorkspacePVC: "agent-fleet-workspace",
	}
}

func TestCreatePod_ShapeMatchesTSVersion(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	task := TaskRef{ID: "abc-123-def", Repo: "dream-analyst"}

	if err := c.CreatePod(ctx, task); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName(task.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[ComponentLabel] != "e2e-runner" || pod.Labels[TaskIDLabel] != task.ID {
		t.Errorf("unexpected labels: %+v", pod.Labels)
	}
	if pod.Spec.RestartPolicy != "Never" {
		t.Errorf("expected RestartPolicy Never, got %s", pod.Spec.RestartPolicy)
	}
	container := pod.Spec.Containers[0]
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[0].SubPath != "worktrees/"+task.ID {
		t.Errorf("unexpected volume mounts: %+v", container.VolumeMounts)
	}
	if container.Ports[0].ContainerPort != AppPort || container.Ports[1].ContainerPort != CodeServerPort || container.Ports[2].ContainerPort != PlaywrightPort {
		t.Errorf("unexpected ports: %+v", container.Ports)
	}
	dshmVol := pod.Spec.Volumes[1]
	if dshmVol.EmptyDir == nil || dshmVol.EmptyDir.Medium != "Memory" || dshmVol.EmptyDir.SizeLimit.String() != "1Gi" {
		t.Errorf("unexpected dshm volume: %+v", dshmVol)
	}
}

func TestCreateService_Shape(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateService(ctx, "task-1"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := c.Core.CoreV1().Services("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[TaskIDLabel] != "task-1" {
		t.Errorf("unexpected selector: %+v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 3 {
		t.Errorf("expected 3 ports, got %d", len(svc.Spec.Ports))
	}
}

func TestCreateMiddleware_StripPrefixPaths(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateMiddleware(ctx, "task-1"); err != nil {
		t.Fatalf("CreateMiddleware: %v", err)
	}
	obj, err := c.Dynamic.Resource(middlewareGVR).Namespace("agent-fleet").Get(ctx, stripPrefixName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get middleware: %v", err)
	}
	prefixes, _, _ := unstructuredNestedSlice(obj.Object, "spec", "stripPrefix", "prefixes")
	if len(prefixes) != 2 || prefixes[0] != "/task-1/app" || prefixes[1] != "/task-1/code" {
		t.Errorf("unexpected prefixes: %+v", prefixes)
	}
}

func TestCreateIngressRoute_Routes(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateIngressRoute(ctx, "e2e.bnei.dev", "task-1"); err != nil {
		t.Fatalf("CreateIngressRoute: %v", err)
	}
	obj, err := c.Dynamic.Resource(ingressRouteGVR).Namespace("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingressroute: %v", err)
	}
	routes, _, _ := unstructuredNestedSlice(obj.Object, "spec", "routes")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	route0 := routes[0].(map[string]any)
	if route0["match"] != "Host(`e2e.bnei.dev`) && PathPrefix(`/task-1/app`)" {
		t.Errorf("unexpected match: %v", route0["match"])
	}
}

func TestDeleteAll_IgnoresNotFound(t *testing.T) {
	c := newTestClient()
	if err := c.DeleteAll(context.Background(), "never-existed"); err != nil {
		t.Fatalf("DeleteAll should ignore 404s, got: %v", err)
	}
}

func TestCreateWorkerPod_TwoContainersSharedPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main", "/workspace/worktrees/task-1", ""); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Labels[ComponentLabel] != ComponentWorker || job.Labels[RepoLabel] != "dream-analyst" {
		t.Errorf("unexpected labels: %+v", job.Labels)
	}
	// reliability-findings.md #11: core's heartbeat/reclaim stays the sole
	// retry mechanism — a k8s-level retry here would double up against it.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("expected BackoffLimit 0 (no k8s-level retry), got %v", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != workerJobTTLSeconds {
		t.Errorf("expected TTLSecondsAfterFinished %d, got %v", workerJobTTLSeconds, job.Spec.TTLSecondsAfterFinished)
	}
	podSpec := job.Spec.Template.Spec
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Name != "worker" {
		t.Fatalf("expected 1 container (worker), got %+v", podSpec.Containers)
	}
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Name != "sidecar" {
		t.Fatalf("expected 1 init container (sidecar), got %+v", podSpec.InitContainers)
	}
	sidecar := podSpec.InitContainers[0]
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sidecar init container must be a native sidecar (RestartPolicy: Always), got %v", sidecar.RestartPolicy)
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.HTTPGet == nil || sidecar.StartupProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("sidecar init container must have an HTTP /readyz startup probe so the worker waits for a proven core connection, got %+v", sidecar.StartupProbe)
	}
	// Whole PVC, no SubPath: a linked git worktree's .git gitlink is an
	// absolute path back to repos/<repo>/.git/worktrees/<taskId>, which a
	// SubPath scoped to just worktrees/<taskId> would put out of reach.
	for _, ctr := range append(append([]corev1.Container{}, podSpec.Containers...), podSpec.InitContainers...) {
		if len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].SubPath != "" || ctr.VolumeMounts[0].MountPath != "/workspace" {
			t.Errorf("container %s: unexpected volume mounts: %+v", ctr.Name, ctr.VolumeMounts)
		}
		foundWorktreePath := false
		for _, e := range ctr.Env {
			if e.Name == "WORKTREE_PATH" {
				foundWorktreePath = true
				if e.Value != "/workspace/worktrees/task-1" {
					t.Errorf("container %s: unexpected WORKTREE_PATH: %q", ctr.Name, e.Value)
				}
			}
		}
		if !foundWorktreePath {
			t.Errorf("container %s: missing WORKTREE_PATH env var", ctr.Name)
		}
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].PersistentVolumeClaim.ClaimName != "agent-fleet-workspace" {
		t.Errorf("expected single shared-PVC volume, got: %+v", podSpec.Volumes)
	}
}

// findEnv returns the value of the first env var named `name`, and whether
// it was found at all — RESUME_SESSION_ID legitimately has an empty-string
// value for a brand-new task, so "found but empty" and "not set" have to
// be distinguishable.
func findEnv(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestCreateWorkerPod_ResumeSession covers the sessions redesign's resume
// path (supersedes docs/adr/0021/0025's phase-boundary framing, completes
// the CLAUDE_CONFIG_DIR redirect ADR-0016 described but never wired up) —
// CLAUDE_CONFIG_DIR must always point at the shared PVC (or `resume:` has
// nothing to resume from regardless of what session id is passed), and
// RESUME_SESSION_ID must carry through whatever resumeSessionID was given,
// including empty for a fresh task.
func TestCreateWorkerPod_ResumeSession(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main", "/workspace/worktrees/task-1", "sess-abc123"); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	if configDir, ok := findEnv(worker.Env, "CLAUDE_CONFIG_DIR"); !ok || configDir != claudeConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q (found=%v), want %q", configDir, ok, claudeConfigDir)
	}
	if resumeID, ok := findEnv(worker.Env, "RESUME_SESSION_ID"); !ok || resumeID != "sess-abc123" {
		t.Errorf("RESUME_SESSION_ID = %q (found=%v), want %q", resumeID, ok, "sess-abc123")
	}

	// A fresh task (no prior session) must still set RESUME_SESSION_ID —
	// present-but-empty, not omitted, so worker/src/session.ts's env read
	// doesn't have to distinguish "unset" from "empty" itself.
	if err := c.CreateWorkerPod(ctx, "task-2", "dream-analyst", "test task", "lease-2", "main", "/workspace/worktrees/task-2", ""); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job2, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if resumeID, ok := findEnv(job2.Spec.Template.Spec.Containers[0].Env, "RESUME_SESSION_ID"); !ok || resumeID != "" {
		t.Errorf("fresh task RESUME_SESSION_ID = %q (found=%v), want empty-but-present", resumeID, ok)
	}
}

func TestGetPod_ExistsVsNotFound(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	_, exists, err := c.GetPod(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing pod, got exists=%v err=%v", exists, err)
	}

	// GetPod is the e2e-preview-pod getter (worker sessions are Jobs now,
	// reliability-findings.md #11) — exercised against CreatePod, not
	// CreateWorkerPod.
	if err := c.CreatePod(ctx, TaskRef{ID: "task-1", Repo: "dream-analyst"}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	_, exists, err = c.GetPod(ctx, ResourceName("task-1"))
	if err != nil || !exists {
		t.Fatalf("expected exists=true for a created pod, got exists=%v err=%v", exists, err)
	}
}

func TestGetWorkerJobRepo_RecoversRepoFromLabel(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "vos-monolith", "test task", "lease-1", "dev", "/workspace/worktrees/task-1", ""); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	repo, exists, err := c.GetWorkerJobRepo(ctx, "task-1")
	if err != nil || !exists {
		t.Fatalf("expected exists=true, got exists=%v err=%v", exists, err)
	}
	if repo != "vos-monolith" {
		t.Errorf("expected repo=vos-monolith, got %q", repo)
	}

	_, exists, err = c.GetWorkerJobRepo(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing job, got exists=%v err=%v", exists, err)
	}
}

func TestListWorkerJobsByLabel_ExcludesE2ePods(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main", "/workspace/worktrees/task-1", ""); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if err := c.CreatePod(ctx, TaskRef{ID: "task-2", Repo: "dream-analyst"}); err != nil {
		t.Fatalf("CreatePod (e2e): %v", err)
	}

	jobs, err := c.ListWorkerJobsByLabel(ctx)
	if err != nil {
		t.Fatalf("ListWorkerJobsByLabel: %v", err)
	}
	if len(jobs) != 1 || jobs[0].TaskID != "task-1" {
		t.Errorf("expected only the worker job listed, got %+v", jobs)
	}
}

func TestJobPhase_DerivedFromConditions(t *testing.T) {
	cases := []struct {
		name  string
		conds []batchv1.JobCondition
		want  string
	}{
		{"no conditions yet", nil, ""},
		{"complete", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}, "Succeeded"},
		{"failed", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}}, "Failed"},
		{"condition present but not True", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "False"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conds}}
			if got := jobPhase(job); got != tc.want {
				t.Errorf("jobPhase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// small helper avoiding an extra import for one nested-slice read
func unstructuredNestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	cur := obj
	for i, f := range fields {
		if i == len(fields)-1 {
			v, ok := cur[f].([]any)
			return v, ok, nil
		}
		next, ok := cur[f].(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur = next
	}
	return nil, false, nil
}
