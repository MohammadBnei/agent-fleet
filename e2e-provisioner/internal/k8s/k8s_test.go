package k8s

import (
	"context"
	"testing"

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
		Core:        fake.NewSimpleClientset(),
		Dynamic:     dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		Namespace:   "agent-fleet",
		RunnerImage: "mohammaddocker/agent-fleet-e2e-runner:latest",
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
