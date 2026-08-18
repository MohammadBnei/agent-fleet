package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
)

// These tests exercise the idempotent Kubernetes-object-creation building
// blocks against a fake clientset. EnsureSharedInstance's full path
// (including waitReady, a real network connection attempt) needs an actual
// running Postgres/Redis — that's covered by kind-local + credentials.go's
// integration test, not a fake-clientset unit test (docs/adr/0034).

func TestEnsureAdminSecret_IdempotentAndStable(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	password1, err := c.ensureAdminSecret(ctx, "dream-analyst", "postgres")
	if err != nil {
		t.Fatalf("ensureAdminSecret: %v", err)
	}
	if password1 == "" {
		t.Fatal("ensureAdminSecret returned empty password")
	}

	password2, err := c.ensureAdminSecret(ctx, "dream-analyst", "postgres")
	if err != nil {
		t.Fatalf("second ensureAdminSecret: %v", err)
	}
	if password1 != password2 {
		t.Errorf("password changed across calls: %q vs %q", password1, password2)
	}

	secret, err := c.Core.CoreV1().Secrets(c.Namespace).Get(ctx, SharedInstanceAdminSecretName("dream-analyst", "postgres"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(secret.Data["password"]) != password1 {
		t.Errorf("secret data = %q, want %q", secret.Data["password"], password1)
	}
}

func TestEnsureSharedDeployment_PostgresGetsPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "s3cr3t"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}

	dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *dep.Spec.Replicas)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 || dep.Spec.Template.Spec.Containers[0].Image != c.PostgresImage {
		t.Errorf("unexpected containers: %+v", dep.Spec.Template.Spec.Containers)
	}
	if len(dep.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("expected 1 volume (PVC-backed data dir), got %d", len(dep.Spec.Template.Spec.Volumes))
	}

	// Regression test: found live — mounting the PVC directly at postgres's
	// default PGDATA makes initdb fail the moment the volume's filesystem
	// has a lost+found dir at its root (standard for a freshly-formatted
	// volume). PGDATA must point at a subdirectory of the mount, which
	// starts genuinely empty regardless of what's alongside it.
	container := dep.Spec.Template.Spec.Containers[0]
	foundPGDATA := false
	for _, e := range container.Env {
		if e.Name == "PGDATA" {
			foundPGDATA = true
			if e.Value == postgresDataMountPath {
				t.Errorf("PGDATA = %q, must be a SUBDIRECTORY of the mount path, not the mount path itself", e.Value)
			}
		}
	}
	if !foundPGDATA {
		t.Error("expected a PGDATA env var pointing at a subdirectory of the PVC mount")
	}

	if _, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("expected a PVC for postgres, get failed: %v", err)
	}

	// Second call must be a no-op, not an error or a duplicate object.
	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "s3cr3t"); err != nil {
		t.Fatalf("second ensureSharedDeployment: %v", err)
	}
}

func TestEnsureSharedDeployment_RedisNoPVCRequirepass(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "redis")
	labels := SharedInstanceLabels("dream-analyst", "redis")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["redis"], c.RedisImage, "redis", "s3cr3t"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}

	dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("expected no volumes for redis, got %d", len(dep.Spec.Template.Spec.Volumes))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	found := false
	for _, arg := range container.Command {
		if arg == "s3cr3t" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --requirepass password in container.Command, got %v", container.Command)
	}

	if _, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("expected no PVC for redis, but one exists")
	}
}

func TestEnsureSharedService_Idempotent(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	if err := c.ensureSharedService(ctx, name, labels, catalog.Services["postgres"].Port); err != nil {
		t.Fatalf("ensureSharedService: %v", err)
	}
	if err := c.ensureSharedService(ctx, name, labels, catalog.Services["postgres"].Port); err != nil {
		t.Fatalf("second ensureSharedService: %v", err)
	}

	svc, err := c.Core.CoreV1().Services(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != catalog.Services["postgres"].Port {
		t.Errorf("unexpected ports: %+v", svc.Spec.Ports)
	}
}

func TestTouchLastUsedAt_UpdatesAnnotation(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "s3cr3t"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}
	if err := c.touchLastUsedAt(ctx, name); err != nil {
		t.Fatalf("touchLastUsedAt: %v", err)
	}

	dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Template.Annotations[LastUsedAtAnnotation] == "" {
		t.Error("expected last-used-at annotation to be set")
	}
}

// TestDescribeSharedInstanceFailure_SurfacesContainerWaitingReason covers
// the fix for a real observability gap found live: EnsureSharedInstance's
// timeout error used to carry only the bare connection-level symptom
// ("connection refused"), never why the instance pod itself was actually
// failing (e.g. postgres crash-looping on an initdb error) — an operator
// had to go find that manually via a separate kubectl invocation. This
// asserts the container's own waiting reason/message end up in the
// description string that now gets folded into the returned error.
func TestDescribeSharedInstanceFailure_SurfacesContainerWaitingReason(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-dream-analyst-postgres-abc123", Namespace: c.Namespace, Labels: labels},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "postgres",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CrashLoopBackOff",
						Message: "back-off 40s restarting failed container",
					},
				},
			}},
		},
	}
	if _, err := c.Core.CoreV1().Pods(c.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	desc := c.describeSharedInstanceFailure(ctx, "dream-analyst", "postgres")
	if !strings.Contains(desc, "CrashLoopBackOff") || !strings.Contains(desc, "back-off 40s restarting failed container") {
		t.Errorf("description = %q, want it to contain the container's waiting reason/message", desc)
	}
}

func TestDescribeSharedInstanceFailure_NoPodFound(t *testing.T) {
	c := newTestClient()
	desc := c.describeSharedInstanceFailure(context.Background(), "dream-analyst", "postgres")
	if desc == "" {
		t.Error("expected a non-empty fallback description when no pod exists")
	}
}

func TestEnsureSharedInstance_UnknownServiceKey(t *testing.T) {
	c := newTestClient()
	if _, _, _, err := c.EnsureSharedInstance(context.Background(), "dream-analyst", "not-a-real-service"); err == nil {
		t.Fatal("expected error for unknown service key")
	}
}

// A RollingUpdate against a ReadWriteOnce volume deadlocks forever: the new
// pod waits for a volume the old pod will not release, and the old pod waits
// for the new one to be Ready. Found live on 2026-08-18 with
// svc-agent-fleet-postgres stuck ContainerCreating for 5h16m.
//
// Both services are asserted, not just postgres. Redis has no PVC today
// (catalog.ServiceDef.NeedsPVC is false), but a single-replica stateful
// service is the wrong shape for a rolling update either way, and coupling
// the strategy to NeedsPVC would make this bug reappear the day redis gains
// a volume.
func TestEnsureSharedDeployment_UsesRecreateNotRollingUpdate(t *testing.T) {
	ctx := context.Background()

	for _, serviceKey := range []string{"postgres", "redis"} {
		t.Run(serviceKey, func(t *testing.T) {
			c := newTestClient()
			name := SharedInstanceName("dream-analyst", serviceKey)
			labels := SharedInstanceLabels("dream-analyst", serviceKey)
			image := c.PostgresImage
			if serviceKey == "redis" {
				image = c.RedisImage
			}

			if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services[serviceKey], image, serviceKey, "s3cr3t"); err != nil {
				t.Fatalf("ensureSharedDeployment: %v", err)
			}

			dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get deployment: %v", err)
			}
			if got := dep.Spec.Strategy.Type; got != appsv1.RecreateDeploymentStrategyType {
				t.Errorf("strategy = %q, want %q — a rolling update against an RWO volume never converges",
					got, appsv1.RecreateDeploymentStrategyType)
			}
		})
	}
}
