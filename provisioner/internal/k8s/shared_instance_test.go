package k8s

import (
	"context"
	"testing"

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

func TestEnsureSharedInstance_UnknownServiceKey(t *testing.T) {
	c := newTestClient()
	if _, _, _, err := c.EnsureSharedInstance(context.Background(), "dream-analyst", "not-a-real-service"); err == nil {
		t.Fatal("expected error for unknown service key")
	}
}
