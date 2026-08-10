package k8s

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
)

// sharedInstanceReadyTimeout bounds how long EnsureSharedInstance waits for
// a freshly-created (or still-starting) instance to accept connections —
// this doubles as the readiness check itself (a real connection attempt),
// no separate k8s-Pod-readiness poll needed (docs/adr/0034).
const sharedInstanceReadyTimeout = 60 * time.Second

// EnsureSharedInstance idempotently materializes the per-(repo, serviceKey)
// shared instance (Secret + Deployment + optional PVC + Service) used by
// task-scoped/repo-scoped service ingredients, and blocks until it's
// actually reachable. Safe to call repeatedly — a second call against an
// already-existing instance is a fast no-op past the GET-before-create
// checks, mirroring CreateE2eSession's existing idempotent idiom.
func (c *Client) EnsureSharedInstance(ctx context.Context, repo, serviceKey string) (host string, port int32, adminPassword string, err error) {
	def, ok := catalog.Services[serviceKey]
	if !ok {
		return "", 0, "", fmt.Errorf("unknown service ingredient %q", serviceKey)
	}
	name := SharedInstanceName(repo, serviceKey)
	labels := SharedInstanceLabels(repo, serviceKey)

	adminPassword, err = c.ensureAdminSecret(ctx, repo, serviceKey)
	if err != nil {
		return "", 0, "", fmt.Errorf("ensure admin secret: %w", err)
	}

	image := c.serviceImage(serviceKey)
	if err := c.ensureSharedDeployment(ctx, name, labels, def, image, serviceKey, adminPassword); err != nil {
		return "", 0, "", fmt.Errorf("ensure shared deployment: %w", err)
	}
	if err := c.ensureSharedService(ctx, name, labels, def.Port); err != nil {
		return "", 0, "", fmt.Errorf("ensure shared service: %w", err)
	}

	host = fmt.Sprintf("%s.%s.svc.cluster.local", name, c.Namespace)
	if err := waitReady(ctx, serviceKey, def, host, adminPassword); err != nil {
		return "", 0, "", fmt.Errorf("wait for %s ready: %w", serviceKey, err)
	}

	if err := c.touchLastUsedAt(ctx, name); err != nil {
		// Non-fatal: a missed annotation touch just makes this instance a
		// slightly earlier idle-GC candidate than it should be, it doesn't
		// break the caller's actual request.
		slog.Warn("touch last-used-at failed", "name", name, "error", err)
	}

	slog.Info("k8s EnsureSharedInstance", "repo", repo, "serviceKey", serviceKey, "host", host)
	return host, def.Port, adminPassword, nil
}

func (c *Client) serviceImage(serviceKey string) string {
	if serviceKey == "redis" {
		return c.RedisImage
	}
	return c.PostgresImage
}

// ensureAdminSecret returns the admin password, generating and storing a
// new random one the first time this (repo, serviceKey) pair is ever
// requested — the provisioner sets its own admin credentials for
// infrastructure it itself created, distinct from and never touching
// AGENTFLEET_DB_* (docs/adr/0020 point 1 stays intact).
func (c *Client) ensureAdminSecret(ctx context.Context, repo, serviceKey string) (string, error) {
	name := SharedInstanceAdminSecretName(repo, serviceKey)
	secrets := c.Core.CoreV1().Secrets(c.Namespace)

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return string(existing.Data["password"]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	password, err := randomPassword()
	if err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: SharedInstanceLabels(repo, serviceKey)},
		// Data (base64-native), not StringData: a real apiserver merges
		// StringData into Data on write, but a fake clientset in tests
		// doesn't replicate that admission-layer behavior — writing Data
		// directly is correct against both.
		Data: map[string][]byte{"password": []byte(password)},
	}
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a create race against another concurrent request for the
			// same instance — re-read whatever the winner wrote.
			existing, getErr := secrets.Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return "", getErr
			}
			return string(existing.Data["password"]), nil
		}
		return "", err
	}
	return password, nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (c *Client) ensureSharedDeployment(ctx context.Context, name string, labels map[string]string, def catalog.ServiceDef, image, serviceKey, adminPassword string) error {
	deployments := c.Core.AppsV1().Deployments(c.Namespace)
	if _, err := deployments.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	if def.NeedsPVC {
		if err := c.ensureSharedPVC(ctx, name, labels); err != nil {
			return fmt.Errorf("ensure pvc: %w", err)
		}
	}

	container := corev1.Container{
		Name:  serviceKey,
		Image: image,
		Ports: []corev1.ContainerPort{{ContainerPort: def.Port}},
		Env:   serviceEnv(serviceKey, def, adminPassword),
	}
	if serviceKey == "redis" {
		// redis:7-alpine has no POSTGRES_PASSWORD-style env var for this —
		// --requirepass on the command line is the documented way to set
		// it at start time.
		container.Command = []string{"redis-server", "--requirepass", adminPassword}
	}
	volumes := []corev1.Volume{}
	if def.NeedsPVC {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: postgresDataMountPath}}
		volumes = append(volumes, corev1.Volume{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}},
		})
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{LastUsedAtAnnotation: nowRFC3339()}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}
	_, err := deployments.Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil // lost a create race, the winner's object is fine
	}
	return err
}

// postgresDataMountPath matches the postgres image's default PGDATA
// location — no PGDATA env override needed.
const postgresDataMountPath = "/var/lib/postgresql/data"

func serviceEnv(serviceKey string, def catalog.ServiceDef, adminPassword string) []corev1.EnvVar {
	if serviceKey == "redis" {
		// redis:7-alpine has no built-in "start with this password" env var
		// (unlike postgres's POSTGRES_PASSWORD) — --requirepass is passed
		// as a command-line arg instead, see ensureSharedDeployment's
		// container.Command wiring below. No env vars needed here.
		return nil
	}
	return []corev1.EnvVar{
		{Name: "POSTGRES_USER", Value: def.AdminUser},
		{Name: "POSTGRES_PASSWORD", Value: adminPassword},
		{Name: "POSTGRES_DB", Value: "postgres"},
	}
}

func (c *Client) ensureSharedPVC(ctx context.Context, name string, labels map[string]string) error {
	pvcs := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace)
	if _, err := pvcs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resourcePtr(c.SharedInstancePVCSize)},
			},
		},
	}
	_, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *Client) ensureSharedService(ctx context.Context, name string, labels map[string]string, port int32) error {
	services := c.Core.CoreV1().Services(c.Namespace)
	if _, err := services.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(port)}},
		},
	}
	_, err := services.Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// touchLastUsedAt marks this instance as used right now — the idle-GC
// substrate (docs/adr/0034 §"Idle-timeout GC"). Called on every
// EnsureSharedInstance, first-use and every reuse alike.
func (c *Client) touchLastUsedAt(ctx context.Context, name string) error {
	deployments := c.Core.AppsV1().Deployments(c.Namespace)
	dep, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[LastUsedAtAnnotation] = nowRFC3339()
	_, err = deployments.Update(ctx, dep, metav1.UpdateOptions{})
	return err
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// LiveSharedInstance is the reconcile loop's idle-GC view of a shared
// instance (docs/adr/0034) — Kubernetes itself is the source of truth,
// same reasoning as LiveWorkerJob: the provisioner holds no DB to track
// this in separately.
type LiveSharedInstance struct {
	Repo       string
	ServiceKey string
	// LastUsedAt is the zero time if the annotation is missing or
	// unparseable — the reconcile loop treats that as "don't GC yet" (a
	// brand-new instance mid-creation shouldn't be swept just because it
	// hasn't been touched a second time yet).
	LastUsedAt time.Time
}

// ListSharedInstances lists every shared-instance Deployment across all
// repos/services — the reconcile loop's gcIdleSharedInstances pass filters
// by LastUsedAt itself.
func (c *Client) ListSharedInstances(ctx context.Context) ([]LiveSharedInstance, error) {
	list, err := c.Core.AppsV1().Deployments(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ComponentLabel + "=" + ComponentSharedInstance,
	})
	if err != nil {
		slog.Error("k8s ListSharedInstances", "error", err)
		return nil, err
	}
	out := make([]LiveSharedInstance, 0, len(list.Items))
	for _, dep := range list.Items {
		repo := dep.Labels[RepoLabel]
		serviceKey := dep.Labels[ServiceKeyLabel]
		if repo == "" || serviceKey == "" {
			continue
		}
		var lastUsed time.Time
		if raw := dep.Spec.Template.Annotations[LastUsedAtAnnotation]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				lastUsed = t
			}
		}
		out = append(out, LiveSharedInstance{Repo: repo, ServiceKey: serviceKey, LastUsedAt: lastUsed})
	}
	return out, nil
}

// DeleteSharedInstance tears down every object EnsureSharedInstance may
// have created for (repo, serviceKey) — Deployment, Service, admin Secret,
// and (postgres only) its PVC. Ignores not-found on each so a partially
// materialized instance (e.g. a failed EnsureSharedInstance that got as
// far as the Deployment but not the Service) is still fully cleaned up.
func (c *Client) DeleteSharedInstance(ctx context.Context, repo, serviceKey string) error {
	name := SharedInstanceName(repo, serviceKey)
	if err := ignoreNotFound(c.Core.AppsV1().Deployments(c.Namespace).Delete(ctx, name, jobForegroundDeletion())); err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if err := ignoreNotFound(c.Core.CoreV1().Services(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := ignoreNotFound(c.Core.CoreV1().Secrets(c.Namespace).Delete(ctx, SharedInstanceAdminSecretName(repo, serviceKey), metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete admin secret: %w", err)
	}
	if err := ignoreNotFound(c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete pvc: %w", err)
	}
	slog.Info("k8s DeleteSharedInstance", "repo", repo, "serviceKey", serviceKey)
	return nil
}

// waitReady blocks until the shared instance actually accepts connections
// — a real connection attempt IS the readiness check (docs/adr/0034), not
// a separate probe/poll layered on top.
func waitReady(ctx context.Context, serviceKey string, def catalog.ServiceDef, host, adminPassword string) error {
	deadline := time.Now().Add(sharedInstanceReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var err error
		if serviceKey == "redis" {
			err = pingRedis(ctx, host, def.Port, adminPassword)
		} else {
			err = pingPostgres(ctx, host, def.Port, def.AdminUser, adminPassword)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out after %s, last error: %w", sharedInstanceReadyTimeout, lastErr)
}

func pingPostgres(ctx context.Context, host string, port int32, user, password string) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/postgres?sslmode=disable", user, password, host, port)
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(dialCtx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(dialCtx) }()
	return conn.Ping(dialCtx)
}

// pingRedis speaks just enough RESP to AUTH+PING — no redis client
// dependency needed for two commands (ladder: stdlib net.Conn is enough).
func pingRedis(ctx context.Context, host string, port int32, password string) error {
	conn, err := dialRedisAuthenticated(ctx, host, port, password)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(respCommand("PING")); err != nil {
		return err
	}
	return readRESPStatus(conn)
}

// dialRedisAuthenticated dials and AUTHs, leaving the connection open for
// the caller to issue further commands on (credentials.go's mintRedis) —
// pingRedis above is just AUTH followed by one more command (PING).
func dialRedisAuthenticated(ctx context.Context, host string, port int32, password string) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write(respCommand("AUTH", password)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := readRESPStatus(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("AUTH: %w", err)
	}
	return conn, nil
}

func respCommand(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// readRESPStatus reads one reply line and errors on RESP's "-" error
// prefix — enough to detect a rejected AUTH or a not-yet-ready server
// without a full RESP parser.
func readRESPStatus(conn net.Conn) error {
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return err
	}
	if n > 0 && buf[0] == '-' {
		return fmt.Errorf("redis error: %s", string(buf[1:n]))
	}
	return nil
}
