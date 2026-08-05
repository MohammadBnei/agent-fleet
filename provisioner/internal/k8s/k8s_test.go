package k8s

import (
	"context"
	"testing"

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

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main"); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[ComponentLabel] != ComponentWorker || pod.Labels[RepoLabel] != "dream-analyst" {
		t.Errorf("unexpected labels: %+v", pod.Labels)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "worker" {
		t.Fatalf("expected 1 container (worker), got %+v", pod.Spec.Containers)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "sidecar" {
		t.Fatalf("expected 1 init container (sidecar), got %+v", pod.Spec.InitContainers)
	}
	sidecar := pod.Spec.InitContainers[0]
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sidecar init container must be a native sidecar (RestartPolicy: Always), got %v", sidecar.RestartPolicy)
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.TCPSocket == nil {
		t.Errorf("sidecar init container must have a TCP startup probe so the worker waits for it to be ready, got %+v", sidecar.StartupProbe)
	}
	for _, ctr := range append(append([]corev1.Container{}, pod.Spec.Containers...), pod.Spec.InitContainers...) {
		if len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].SubPath != "worktrees/task-1" {
			t.Errorf("container %s: unexpected volume mounts: %+v", ctr.Name, ctr.VolumeMounts)
		}
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "agent-fleet-workspace" {
		t.Errorf("expected single shared-PVC volume, got: %+v", pod.Spec.Volumes)
	}
}

func TestGetPod_ExistsVsNotFound(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	_, exists, err := c.GetPod(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing pod, got exists=%v err=%v", exists, err)
	}

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main"); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	// Phase itself isn't asserted here: the fake clientset stores exactly
	// what Create() was given and doesn't simulate the real API server's
	// admission-time defaulting (a live cluster sets a fresh pod's phase to
	// Pending; the fake leaves it "", the struct's zero value). exists=true
	// is the property this package actually needs GetPod to be correct
	// about — see e2eStatusFromPhase's own default case for how an
	// unexpected/empty phase is handled downstream.
	_, exists, err = c.GetPod(ctx, WorkerResourceName("task-1"))
	if err != nil || !exists {
		t.Fatalf("expected exists=true for a created pod, got exists=%v err=%v", exists, err)
	}
}

func TestGetWorkerPodRepo_RecoversRepoFromLabel(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "vos-monolith", "test task", "lease-1", "dev"); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	repo, exists, err := c.GetWorkerPodRepo(ctx, "task-1")
	if err != nil || !exists {
		t.Fatalf("expected exists=true, got exists=%v err=%v", exists, err)
	}
	if repo != "vos-monolith" {
		t.Errorf("expected repo=vos-monolith, got %q", repo)
	}

	_, exists, err = c.GetWorkerPodRepo(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing pod, got exists=%v err=%v", exists, err)
	}
}

func TestListWorkerPodsByLabel_ExcludesE2ePods(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "test task", "lease-1", "main"); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if err := c.CreatePod(ctx, TaskRef{ID: "task-2", Repo: "dream-analyst"}); err != nil {
		t.Fatalf("CreatePod (e2e): %v", err)
	}

	pods, err := c.ListWorkerPodsByLabel(ctx)
	if err != nil {
		t.Fatalf("ListWorkerPodsByLabel: %v", err)
	}
	if len(pods) != 1 || pods[0].TaskID != "task-1" {
		t.Errorf("expected only the worker pod listed, got %+v", pods)
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
