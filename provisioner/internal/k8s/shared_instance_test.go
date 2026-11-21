package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
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

// SharedInstancePVCSize reaches the shared instance's PVC as the request it was configured as, and a size no
// quantity parses from is an error, not the panic resource.MustParse made of it at the first Postgres shared instance.
func TestEnsureSharedPVC_Size(t *testing.T) {
	ctx := context.Background()
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	c := newTestClient()
	c.SharedInstancePVCSize = "9Gi"
	if err := c.ensureSharedPVC(ctx, "pg", labels); err != nil {
		t.Fatalf("ensureSharedPVC: %v", err)
	}
	pvc, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(ctx, "pg", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "9Gi" {
		t.Errorf("requested storage %s, want 9Gi", got.String())
	}

	for _, bad := range []string{"garbage", "", "2 Gi"} {
		c := newTestClient()
		c.SharedInstancePVCSize = bad
		err := c.ensureSharedPVC(ctx, "pg", labels) // a panic fails the test binary
		if err == nil || !strings.Contains(err.Error(), "shared instance pvc size") {
			t.Errorf("size %q: err %v, want an error naming the size", bad, err)
		}
		if _, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(ctx, "pg", metav1.GetOptions{}); err == nil {
			t.Errorf("size %q: a PVC was created", bad)
		}
		// Through the deployment that wants it: the error is the caller's, and no Deployment is left behind.
		name := SharedInstanceName("dream-analyst", "postgres")
		if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err == nil || !strings.Contains(err.Error(), "ensure pvc") {
			t.Errorf("size %q: ensureSharedDeployment err %v, want the pvc's", bad, err)
		}
		if _, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Errorf("size %q: a Deployment was created without its PVC", bad)
		}
	}
}

// The path from the environment: SHARED_INSTANCE_PVC_SIZE=garbage is replaced by the default in config.Load, and the
// shared instance that the client then builds asks for the default (main's images() hands this field over as is).
func TestEnsureSharedPVC_BadEnvSizeBecomesTheDefault(t *testing.T) {
	cfg, err := config.Load(func(k string) string {
		if k == "SHARED_INSTANCE_PVC_SIZE" {
			return "garbage"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient()
	c.SharedInstancePVCSize = cfg.SharedInstancePVCSize
	name := SharedInstanceName("dream-analyst", "postgres")
	if err := c.ensureSharedDeployment(context.Background(), name, SharedInstanceLabels("dream-analyst", "postgres"), catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}
	pvc, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Errorf("requested storage %s, want 2Gi", got.String())
	}
}

func TestEnsureSharedDeployment_PostgresGetsPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err != nil {
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
	if len(dep.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 volumes (admin Secret, PVC-backed data dir), got %d", len(dep.Spec.Template.Spec.Volumes))
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
	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err != nil {
		t.Fatalf("second ensureSharedDeployment: %v", err)
	}
}

func TestEnsureSharedDeployment_RedisNoPVCRequirepass(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "redis")
	labels := SharedInstanceLabels("dream-analyst", "redis")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["redis"], c.RedisImage, "redis", "admin-secret"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}

	dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("expected only the admin Secret volume for redis, got %d", len(dep.Spec.Template.Spec.Volumes))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	if len(container.Command) != 3 || container.Command[2] != redisStart {
		t.Errorf("expected redis started by redisStart, got %v", container.Command)
	}

	if _, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("expected no PVC for redis, but one exists")
	}
}

// docs/adr/0086 §6: the admin password is in no pod spec, only in its Secret,
// mounted as a file with no subPath.
func TestEnsureSharedDeployment_AdminPasswordNotInPodSpec(t *testing.T) {
	for _, key := range []string{"postgres", "redis"} {
		c := newTestClient()
		ctx := context.Background()
		password, err := c.ensureAdminSecret(ctx, "dream-analyst", key)
		if err != nil {
			t.Fatalf("%s: ensureAdminSecret: %v", key, err)
		}
		name := SharedInstanceName("dream-analyst", key)
		secret := SharedInstanceAdminSecretName("dream-analyst", key)
		if err := c.ensureSharedDeployment(ctx, name, SharedInstanceLabels("dream-analyst", key), catalog.Services[key], c.serviceImage(key), key, secret); err != nil {
			t.Fatalf("%s: ensureSharedDeployment: %v", key, err)
		}
		dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPasswordFromFile(t, key, dep, password, secret)
	}
}

// assertPasswordFromFile: the admin password is nowhere in the Deployment, its
// Secret's password key is mounted with no subPath, and the service is told
// to read it from there.
func assertPasswordFromFile(t *testing.T, key string, dep *appsv1.Deployment, password, secret string) {
	t.Helper()
	if raw, _ := json.Marshal(dep); strings.Contains(string(raw), password) {
		t.Errorf("%s: admin password appears in the Deployment", key)
	}
	ps := dep.Spec.Template.Spec
	ct := ps.Containers[0]
	mounted := false
	for _, m := range ct.VolumeMounts {
		mounted = mounted || (m.MountPath == credDir && m.SubPath == "" && m.ReadOnly)
	}
	fromSecret := false
	for _, v := range ps.Volumes {
		fromSecret = fromSecret || (v.Secret != nil && v.Secret.SecretName == secret &&
			slices.ContainsFunc(v.Secret.Items, func(i corev1.KeyToPath) bool { return i.Key == "password" && i.Path == "password" }))
	}
	if !mounted || !fromSecret {
		t.Errorf("%s: admin Secret mounted=%v fromSecret(password key)=%v", key, mounted, fromSecret)
	}
	env := map[string]string{}
	for _, e := range ct.Env {
		env[e.Name] = e.Value
	}
	switch key {
	case "postgres":
		if got := env["POSTGRES_PASSWORD_FILE"]; got != credDir+"/password" {
			t.Errorf("POSTGRES_PASSWORD_FILE = %q, want %q (initdb fails without it)", got, credDir+"/password")
		}
		if _, ok := env["POSTGRES_PASSWORD"]; ok {
			t.Error("POSTGRES_PASSWORD env var still set")
		}
	case "redis":
		if len(ct.Command) != 3 || ct.Command[2] != redisStart {
			t.Errorf("redis command = %v, want redisStart", ct.Command)
		}
	}
}

// An instance created before ADR-0086 §6 carries its admin password in the pod
// spec. ensureSharedDeployment never updates, so touchLastUsedAt rewrites it.
func TestTouchLastUsedAt_MovesAnInlinePasswordToAFile(t *testing.T) {
	for _, key := range []string{"postgres", "redis"} {
		c := newTestClient()
		ctx := context.Background()
		password, err := c.ensureAdminSecret(ctx, "dream-analyst", key)
		if err != nil {
			t.Fatal(err)
		}
		name := SharedInstanceName("dream-analyst", key)
		labels := SharedInstanceLabels("dream-analyst", key)
		secret := SharedInstanceAdminSecretName("dream-analyst", key)
		// The shape the old code created.
		ct := corev1.Container{Name: key, Image: "old-image:1", Ports: []corev1.ContainerPort{{ContainerPort: catalog.Services[key].Port}}}
		var volumes []corev1.Volume
		if key == "postgres" {
			ct.Env = []corev1.EnvVar{
				{Name: "POSTGRES_USER", Value: catalog.Services[key].AdminUser},
				{Name: "POSTGRES_PASSWORD", Value: password},
				{Name: "POSTGRES_DB", Value: "postgres"},
				{Name: "PGDATA", Value: postgresPGDATA},
			}
			ct.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: postgresDataMountPath}}
			volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}
		} else {
			ct.Command = []string{"redis-server", "--requirepass", password}
		}
		old := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(1),
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{ct}, Volumes: volumes},
				},
			},
		}
		old.UID = "dep-uid"
		deployments := c.Core.AppsV1().Deployments(c.Namespace)
		if _, err := deployments.Create(ctx, old, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		// The ReplicaSet the controller made from that template.
		if _, err := c.Core.AppsV1().ReplicaSets(c.Namespace).Create(ctx, ownedReplicaSet(old, "old"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}

		if err := c.touchLastUsedAt(ctx, name, key, secret); err != nil {
			t.Fatalf("%s: touchLastUsedAt: %v", key, err)
		}
		dep, err := deployments.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		assertPasswordFromFile(t, key, dep, password, secret)
		// The old ReplicaSet still runs the pod the Recreate rollout waits
		// on: the touch must leave it alone (the prune comes after the rollout).
		if _, err := c.Core.AppsV1().ReplicaSets(c.Namespace).Get(ctx, name+"-old", metav1.GetOptions{}); err != nil {
			t.Errorf("%s: touch deleted the ReplicaSet still part of the rollout: %v", key, err)
		}
		if l := dep.Spec.RevisionHistoryLimit; l == nil || *l != 0 {
			t.Errorf("%s: revisionHistoryLimit = %v, want 0 (an old ReplicaSet keeps its template)", key, l)
		}
		if got := dep.Spec.Template.Spec.Containers[0].Image; got != "old-image:1" {
			t.Errorf("%s: image = %q, the heal must not also bump the image", key, got)
		}
		if key == "postgres" && !slices.ContainsFunc(dep.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == name
		}) {
			t.Error("postgres data volume lost in the heal")
		}

		// Healed once: the next touch leaves the template alone.
		before := dep.Spec.Template.DeepCopy()
		if err := c.touchLastUsedAt(ctx, name, key, secret); err != nil {
			t.Fatal(err)
		}
		after, _ := deployments.Get(ctx, name, metav1.GetOptions{})
		if !apiequality.Semantic.DeepEqual(before, &after.Spec.Template) {
			t.Errorf("%s: second touch changed the pod template again", key)
		}
	}
}

// The process that reads the admin password file can read it.
//
// ASSUMPTION, not checked by this test: the images run their entrypoint as
// root and the root process reads the file. postgres:16-alpine sets no USER;
// its docker-entrypoint.sh reads POSTGRES_PASSWORD_FILE as root, then drops to
// the postgres user (gosu/su-exec). redis:7-alpine sets no USER, and redisStart
// replaces its entrypoint, so the sh reading the file stays root. The file is
// root-owned, so the owner read bit is what matters; a non-root user needs a
// matching fsGroup and the group bit. The assumption holds for those two
// images only, so the test fails when a default image changes: re-check the
// new image's USER and entrypoint, then update the pin.
func TestSharedPodSpec_AdminPasswordReadableByTheProcess(t *testing.T) {
	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{"postgres": "postgres:16-alpine", "redis": "redis:7-alpine"}
	if cfg.PostgresImage != pinned["postgres"] || cfg.RedisImage != pinned["redis"] {
		t.Fatalf("default images are %q, %q; this test's root-reads-the-file assumption is pinned to %q, %q",
			cfg.PostgresImage, cfg.RedisImage, pinned["postgres"], pinned["redis"])
	}
	for _, key := range []string{"postgres", "redis"} {
		ps := sharedPodSpec("n", catalog.Services[key], pinned[key], key, "admin-secret")
		var mode int32
		for _, v := range ps.Volumes {
			if v.Name == "cred" {
				mode = *v.Secret.DefaultMode
			}
		}
		ct := ps.Containers[0]
		var uid *int64
		if ps.SecurityContext != nil && ps.SecurityContext.RunAsUser != nil {
			uid = ps.SecurityContext.RunAsUser
		}
		if ct.SecurityContext != nil && ct.SecurityContext.RunAsUser != nil {
			uid = ct.SecurityContext.RunAsUser
		}
		switch {
		case uid == nil || *uid == 0:
			if mode&0o400 == 0 {
				t.Errorf("%s: runs as root, password file mode %o has no owner read", key, mode)
			}
		case ps.SecurityContext.FSGroup == nil || mode&0o040 == 0:
			t.Errorf("%s: runs as uid %d, password file needs an fsGroup and group read (mode %o)", key, *uid, mode)
		}
		if key == "redis" && (len(ct.Command) == 0 || ct.Command[0] != "sh") {
			t.Errorf("redis: command %v no longer replaces the entrypoint, so who reads the file changed", ct.Command)
		}
	}
}

func ownedReplicaSet(dep *appsv1.Deployment, hash string) *appsv1.ReplicaSet {
	tmpl := *dep.Spec.Template.DeepCopy()
	tmpl.Labels = map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: hash}
	for k, v := range dep.Spec.Template.Labels {
		tmpl.Labels[k] = v
	}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: dep.Name + "-" + hash, Namespace: dep.Namespace, Labels: tmpl.Labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(dep, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: int32Ptr(1), Selector: dep.Spec.Selector, Template: tmpl},
	}
}

func replicaSetPod(rs *appsv1.ReplicaSet) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: rs.Name + "-pod", Namespace: rs.Namespace, Labels: rs.Spec.Template.Labels}}
}

// assertNoReplicaSetCarries: no ReplicaSet's template holds the admin password.
func assertNoReplicaSetCarries(t *testing.T, c *Client, password string) {
	t.Helper()
	rss, err := c.Core.AppsV1().ReplicaSets(c.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rs := range rss.Items {
		if raw, _ := json.Marshal(rs); strings.Contains(string(raw), password) {
			t.Errorf("ReplicaSet %s still carries the admin password", rs.Name)
		}
	}
}

// waitRolledOut returns only once the controller has seen the spec, every
// replica is updated and ready, and no pod of an older ReplicaSet is left.
func TestWaitRolledOut(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "redis")
	labels := SharedInstanceLabels("dream-analyst", "redis")
	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["redis"], c.RedisImage, "redis", "admin-secret"); err != nil {
		t.Fatal(err)
	}
	deployments := c.Core.AppsV1().Deployments(c.Namespace)
	dep, _ := deployments.Get(ctx, name, metav1.GetOptions{})
	dep.UID, dep.Generation = "dep-uid", 2
	newRS := ownedReplicaSet(dep, "new")
	oldRS := ownedReplicaSet(dep, "old")
	oldRS.Spec.Template.Spec.Containers[0].Image = "redis:6"
	oldPod := replicaSetPod(oldRS)
	for _, o := range []*appsv1.ReplicaSet{newRS, oldRS} {
		if _, err := c.Core.AppsV1().ReplicaSets(c.Namespace).Create(ctx, o, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pods := c.Core.CoreV1().Pods(c.Namespace)
	complete := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
	now := metav1.Now()
	for _, tc := range []struct {
		name   string
		status appsv1.DeploymentStatus
		pod    *corev1.Pod
		done   bool
	}{
		{"spec not yet observed", appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}, replicaSetPod(newRS), false},
		{"old pod still the one", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 0, ReadyReplicas: 1}, oldPod, false},
		{"updated, not ready", appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1}, replicaSetPod(newRS), false},
		{"status complete, old pod left", complete, oldPod, false},
		{"new pod terminating", complete, func() *corev1.Pod { p := replicaSetPod(newRS); p.DeletionTimestamp = &now; return p }(), false},
		{"rolled out", complete, replicaSetPod(newRS), true},
	} {
		dep.Status = tc.status
		if _, err := deployments.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{replicaSetPod(newRS).Name, oldPod.Name} {
			_ = pods.Delete(ctx, n, metav1.DeleteOptions{})
		}
		if _, err := pods.Create(ctx, tc.pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		err := c.waitRolledOut(short, name)
		cancel()
		if (err == nil) != tc.done {
			t.Errorf("%s: waitRolledOut err=%v, want done=%v", tc.name, err, tc.done)
		}
	}
}

// EnsureSharedInstance heals, waits for the rollout, then for readiness, and
// only then returns to the minter. Every caller waits: "concurrent caller" is
// one whose own touch changes no template while another caller's heal is
// still rolling out. The waitReady stub records the order; moving the touch
// after the readiness wait, dropping the rollout wait, or deleting the old
// ReplicaSet before its pod is gone breaks it.
func TestEnsureSharedInstance_HealThenRolloutThenReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"healing caller", []string{"heal", "rolled-out", "pruned", "ready"}},
		{"concurrent caller", []string{"rolled-out", "pruned", "ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient()
			cs := c.Core.(*fake.Clientset)
			ctx := context.Background()
			const repo, key = "dream-analyst", "redis"
			password, err := c.ensureAdminSecret(ctx, repo, key)
			if err != nil {
				t.Fatal(err)
			}
			name := SharedInstanceName(repo, key)
			labels := SharedInstanceLabels(repo, key)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels, UID: "dep-uid", Generation: 1},
				Spec: appsv1.DeploymentSpec{
					Replicas: int32Ptr(1),
					Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
					Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: key, Image: c.RedisImage, Command: []string{"redis-server", "--requirepass", password}}}},
					},
				},
				Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
			}
			oldRS := ownedReplicaSet(dep, "old")
			if tc.name == "concurrent caller" {
				// Another caller's heal already landed; its rollout has not.
				dep.Spec.Template.Spec = sharedPodSpec(name, catalog.Services[key], c.RedisImage, key, SharedInstanceAdminSecretName(repo, key))
				dep.Spec.RevisionHistoryLimit = int32Ptr(0)
				dep.Generation = 2
			}
			if _, err := cs.AppsV1().Deployments(c.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := cs.AppsV1().ReplicaSets(c.Namespace).Create(ctx, oldRS, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := cs.CoreV1().Pods(c.Namespace).Create(ctx, replicaSetPod(oldRS), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}

			var events []string
			depGVR := appsv1.SchemeGroupVersion.WithResource("deployments")
			rsGVR := appsv1.SchemeGroupVersion.WithResource("replicasets")
			armed, done, gets := false, false, 0
			// The heal: the API server bumps generation on a spec change.
			cs.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				d := a.(k8stesting.UpdateAction).GetObject().(*appsv1.Deployment)
				if !armed && tc.name == "healing caller" && !inlinePassword(d.Spec.Template.Spec) {
					d.Generation = 2
					events = append(events, "heal")
				}
				armed = true
				return false, nil, nil
			})
			// The controller: the second Deployment read after the touch finds
			// the Recreate rollout finished (old pod gone, new RS and pod up).
			cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
				if !armed || done {
					return false, nil, nil
				}
				if gets++; gets < 2 {
					return false, nil, nil
				}
				done = true
				tr := cs.Tracker()
				obj, err := tr.Get(depGVR, c.Namespace, name)
				if err != nil {
					t.Fatal(err)
				}
				d := obj.(*appsv1.Deployment)
				d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
				newRS := ownedReplicaSet(d, "new")
				scaledDown := oldRS.DeepCopy()
				scaledDown.Spec.Replicas = int32Ptr(0)
				podGVR := corev1.SchemeGroupVersion.WithResource("pods")
				if err := errors.Join(
					tr.Update(depGVR, d, c.Namespace),
					tr.Update(rsGVR, scaledDown, c.Namespace),
					tr.Delete(podGVR, c.Namespace, replicaSetPod(oldRS).Name),
					tr.Create(rsGVR, newRS, c.Namespace),
					tr.Create(podGVR, replicaSetPod(newRS), c.Namespace),
				); err != nil {
					t.Fatal(err)
				}
				events = append(events, "rolled-out")
				return false, nil, nil
			})
			// Deleting the old ReplicaSet before the rollout is complete lets
			// the controller start the new pod while the old one still runs.
			cs.PrependReactor("delete", "replicasets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				obj, err := cs.Tracker().Get(rsGVR, c.Namespace, a.(k8stesting.DeleteAction).GetName())
				if err != nil {
					return false, nil, nil
				}
				rs := obj.(*appsv1.ReplicaSet)
				_, podErr := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), c.Namespace, replicaSetPod(oldRS).Name)
				if !done || rs.Spec.Replicas == nil || *rs.Spec.Replicas != 0 || podErr == nil {
					t.Errorf("ReplicaSet %s deleted while still part of the rollout (rolled out=%v, pod left=%v)", rs.Name, done, podErr == nil)
				}
				events = append(events, "pruned")
				return false, nil, nil
			})
			orig := waitReady
			t.Cleanup(func() { waitReady = orig })
			waitReady = func(context.Context, string, catalog.ServiceDef, string, string) error {
				events = append(events, "ready")
				return nil
			}

			if _, _, got, err := c.EnsureSharedInstance(ctx, repo, key); err != nil || got != password {
				t.Fatalf("EnsureSharedInstance: password match=%v err=%v", got == password, err)
			}
			if !slices.Equal(events, tc.want) {
				t.Errorf("order = %v, want %v", events, tc.want)
			}
			stored, _ := cs.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
			assertPasswordFromFile(t, key, stored, password, SharedInstanceAdminSecretName(repo, key))
			assertNoReplicaSetCarries(t, c, password)
		})
	}
}

// The prune deletes an inline-password ReplicaSet only once it is scaled to
// zero with no pod left; anything else is still part of a rollout.
func TestPruneInlinePasswordReplicaSets_OnlyScaledDownWithNoPod(t *testing.T) {
	for _, tc := range []struct {
		name         string
		spec, status int32
		pod, inline  bool
		wantDeleted  bool
	}{
		{"still scaled up", 1, 1, true, true, false},
		{"scaled down, replicas not yet gone", 0, 1, true, true, false},
		{"scaled down, pod still terminating", 0, 0, true, true, false},
		{"scaled down, no pod", 0, 0, false, true, true},
		{"no inline password", 0, 0, false, false, false},
	} {
		c := newTestClient()
		ctx := context.Background()
		name := SharedInstanceName("dream-analyst", "redis")
		labels := SharedInstanceLabels("dream-analyst", "redis")
		if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["redis"], c.RedisImage, "redis", "admin-secret"); err != nil {
			t.Fatal(err)
		}
		dep, _ := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
		dep.UID = "dep-uid"
		rs := ownedReplicaSet(dep, "old")
		if tc.inline {
			rs.Spec.Template.Spec.Containers[0].Command = []string{"redis-server", "--requirepass", "pw"}
		}
		rs.Spec.Replicas, rs.Status.Replicas = int32Ptr(tc.spec), tc.status
		_, _ = c.Core.AppsV1().Deployments(c.Namespace).Update(ctx, dep, metav1.UpdateOptions{})
		if _, err := c.Core.AppsV1().ReplicaSets(c.Namespace).Create(ctx, rs, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if tc.pod {
			if _, err := c.Core.CoreV1().Pods(c.Namespace).Create(ctx, replicaSetPod(rs), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.pruneInlinePasswordReplicaSets(ctx, name); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, err := c.Core.AppsV1().ReplicaSets(c.Namespace).Get(ctx, rs.Name, metav1.GetOptions{})
		if deleted := err != nil; deleted != tc.wantDeleted {
			t.Errorf("%s: deleted=%v, want %v", tc.name, deleted, tc.wantDeleted)
		}
	}
}

// A heal that hits an update conflict re-reads and lands: the rollout wait
// relies on a nil touch meaning the heal is in.
func TestTouchLastUsedAt_RetriesAConflict(t *testing.T) {
	c := newTestClient()
	cs := c.Core.(*fake.Clientset)
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "redis")
	labels := SharedInstanceLabels("dream-analyst", "redis")
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "redis", Image: c.RedisImage, Command: []string{"redis-server", "--requirepass", "pw"}}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(c.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	conflicts := 1
	cs.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(appsv1.Resource("deployments"), name, errors.New("resourceVersion changed"))
		}
		return false, nil, nil
	})
	if err := c.touchLastUsedAt(ctx, name, "redis", "admin-secret"); err != nil {
		t.Fatalf("touch after one conflict: %v", err)
	}
	got, _ := cs.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if inlinePassword(got.Spec.Template.Spec) {
		t.Error("heal lost to the conflict: template still carries the password inline")
	}
}

// redisStart fails closed and keeps the password out of redis-server's argv.
func TestRedisStart_FailsClosedAndKeepsThePasswordOffArgv(t *testing.T) {
	bin := t.TempDir()
	// A stand-in redis-server that prints its argv and its stdin config.
	stub := "#!/bin/sh\necho \"argv: $*\"\ncat\n"
	if err := os.WriteFile(filepath.Join(bin, "redis-server"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(content *string) (string, error) {
		dir := t.TempDir()
		if content != nil {
			if err := os.WriteFile(filepath.Join(dir, "password"), []byte(*content), 0o400); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("sh", "-c", strings.ReplaceAll(redisStart, credDir, dir))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	pw := "s3cret-Pw_x"
	out, err := run(&pw)
	if err != nil {
		t.Fatalf("good password: %v: %s", err, out)
	}
	if out != "argv: -\nrequirepass "+pw+"\n" {
		t.Errorf("output = %q", out)
	}
	empty := ""
	for name, content := range map[string]*string{"empty": &empty, "missing": nil} {
		if out, err := run(content); err == nil || strings.Contains(out, "argv:") {
			t.Errorf("%s password file: redis-server started (err=%v): %s", name, err, out)
		}
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

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}
	if err := c.touchLastUsedAt(ctx, name, "postgres", "admin-secret"); err != nil {
		t.Fatalf("touchLastUsedAt: %v", err)
	}

	dep, err := c.Core.AppsV1().Deployments(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Annotations[LastUsedAtAnnotation] == "" {
		t.Error("expected last-used-at annotation to be set on the Deployment")
	}
}

// The pod template is the rollout trigger: anything written there mints a
// new pod-template-hash and a new ReplicaSet. last-used-at is touched on
// every single reuse of a shared instance, so putting it there made every
// reuse a rollout — and a single-replica Deployment over a ReadWriteOnce
// volume has no convergent rollout, which is how svc-agent-fleet-postgres
// spent 5h16m in ContainerCreating on 2026-08-18.
//
// This is the assertion that actually prevents the incident. The Recreate
// strategy below only decides what a genuine rollout does; this decides
// whether a routine reuse starts one at all.
func TestTouchLastUsedAt_DoesNotTouchThePodTemplate(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")

	if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services["postgres"], c.PostgresImage, "postgres", "admin-secret"); err != nil {
		t.Fatalf("ensureSharedDeployment: %v", err)
	}
	deployments := c.Core.AppsV1().Deployments(c.Namespace)
	before, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	templateBefore := before.Spec.Template.DeepCopy()

	for i := 0; i < 3; i++ {
		if err := c.touchLastUsedAt(ctx, name, "postgres", "admin-secret"); err != nil {
			t.Fatalf("touchLastUsedAt %d: %v", i, err)
		}
	}

	after, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if !apiequality.Semantic.DeepEqual(templateBefore, &after.Spec.Template) {
		t.Errorf("pod template changed across reuse — every reuse would start a rollout.\nbefore: %+v\nafter:  %+v",
			templateBefore, &after.Spec.Template)
	}
	if _, ok := after.Spec.Template.Annotations[LastUsedAtAnnotation]; ok {
		t.Error("last-used-at is on the pod template; it belongs on the Deployment")
	}
}

// ensureSharedDeployment creates and never updates, so an instance created
// before this fix keeps RollingUpdate forever — including the one found
// wedged live. touchLastUsedAt runs on every reuse and already writes the
// Deployment, so it is where an existing instance gets healed.
func TestTouchLastUsedAt_HealsAPreexistingInstance(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	name := SharedInstanceName("dream-analyst", "postgres")
	labels := SharedInstanceLabels("dream-analyst", "postgres")
	deployments := c.Core.AppsV1().Deployments(c.Namespace)

	// An instance as the old code left it, written the way a real API server
	// hands it back — NOT with an empty strategy. The old code set no
	// strategy, but the API server defaults it, so the object that actually
	// exists carries an explicit RollingUpdate with both parameters. This is
	// the live spec, copied from the wedged Deployment on 2026-08-18.
	//
	// The fake clientset does not default anything, so a fixture with an
	// empty strategy tests a state that cannot occur — and lets a
	// `Type == ""` heal condition pass here while never firing in
	// production. That is exactly what shipped in the first version of this
	// fix.
	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
					MaxUnavailable: &intstr.IntOrString{Type: intstr.String, StrVal: "25%"},
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{LastUsedAtAnnotation: "2026-08-18T05:00:00Z"},
				},
			},
		},
	}
	if _, err := deployments.Create(ctx, old, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create legacy deployment: %v", err)
	}

	// The GC must still see the legacy instance before it is touched,
	// otherwise a never-reused instance reads as the zero time — which the
	// reconcile loop treats as "don't GC yet" — and leaks forever.
	instances, err := c.ListSharedInstances(ctx)
	if err != nil {
		t.Fatalf("ListSharedInstances: %v", err)
	}
	if len(instances) != 1 || instances[0].LastUsedAt.IsZero() {
		t.Fatalf("legacy pod-template timestamp not read back: %+v", instances)
	}

	if err := c.touchLastUsedAt(ctx, name, "postgres", "admin-secret"); err != nil {
		t.Fatalf("touchLastUsedAt: %v", err)
	}

	dep, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := dep.Spec.Strategy.Type; got != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %q, want %q — a pre-existing instance never gets healed otherwise",
			got, appsv1.RecreateDeploymentStrategyType)
	}
	// The API rejects a spec carrying rollingUpdate parameters alongside
	// Recreate, so leaving them behind makes the Update fail outright — and
	// touchLastUsedAt's error is only logged as a warning, so the heal would
	// fail silently on every reuse forever.
	if dep.Spec.Strategy.RollingUpdate != nil {
		t.Errorf("rollingUpdate params left alongside Recreate: %+v — the API rejects that spec",
			dep.Spec.Strategy.RollingUpdate)
	}
	if dep.Annotations[LastUsedAtAnnotation] == "" {
		t.Error("timestamp not migrated onto the Deployment")
	}
	if _, ok := dep.Spec.Template.Annotations[LastUsedAtAnnotation]; ok {
		t.Error("stale timestamp left on the pod template")
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

			if err := c.ensureSharedDeployment(ctx, name, labels, catalog.Services[serviceKey], image, serviceKey, "admin-secret"); err != nil {
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
