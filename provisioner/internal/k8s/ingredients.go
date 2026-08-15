package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
)

// ClusterAccess carries what the cluster-access ingredient's kubectl shim
// needs to reach thot-executor. Passed in rather than read from a global
// so a test can build a pod spec without any ambient config.
type ClusterAccess struct {
	ExecutorAddr string
	AuthToken    string
}

// ServiceIngredientRef is the plain (non-proto) shape CreatePod/
// CreateWorkerPod accept — decouples package k8s from the proto module,
// same reasoning as TaskRef already not being a proto type.
type ServiceIngredientRef struct {
	Key       string
	ScopeMode string
}

// toolsVolumeName/toolsMountPath are shared by every tool ingredient's
// init container and the main container that consumes them.
const (
	toolsVolumeName = "tools"
	toolsMountPath  = "/opt/tools"
)

// buildIngredients turns a resolved profile's tool_keys and pod-scoped
// service ingredients into init containers, a main-container env/PATH,
// and (if any tool ingredient is present) a shared emptyDir volume+mount.
// Task-scoped/repo-scoped service ingredients are NOT handled here — the
// caller (grpcserver.go) mints those separately via MintServiceCredentials
// before pod creation and passes the resulting URL in as extra env
// (docs/adr/0034's fail-loud-before-pod-exists guarantee).
//
// An unknown key fails loud here, before any Kubernetes object is built —
// catching a stale or hand-edited repo_profiles row as early as possible.
func buildIngredients(toolKeys []string, serviceIngredients []ServiceIngredientRef, cluster ClusterAccess) (initContainers []corev1.Container, env []corev1.EnvVar, volume *corev1.Volume, mount *corev1.VolumeMount, err error) {
	for _, key := range toolKeys {
		def, ok := catalog.Tools[key]
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("unknown tool ingredient %q", key)
		}
		initContainers = append(initContainers, corev1.Container{
			Name:  "tool-" + key,
			Image: def.CopyImage,
			// Explicit, not left to Kubernetes' default — that default is
			// per-tag: IfNotPresent for a pinned tag, but Always for
			// `:latest`. Three of the catalog's images are `:latest`
			// (golangci-lint, buf, agent-fleet-executor), so they were
			// re-pulled from Docker Hub on every single pod creation even
			// though the layers were already on the node. That pull is
			// serial and blocks the pod reaching Running, and it spends
			// Docker Hub pull quota per dispatch rather than per node.
			// These images are content-staging only: the init container
			// copies a binary out and exits, so a stale cached layer is
			// exactly as correct as a freshly pulled one.
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         def.CopyCmd,
			VolumeMounts:    []corev1.VolumeMount{{Name: toolsVolumeName, MountPath: toolsMountPath}},
		})
	}
	// cluster-access's shim is useless without knowing where to call and
	// how to authenticate. Set here rather than unconditionally on every
	// worker pod so a normal repo task never carries the executor's
	// token — least privilege by construction, not by convention.
	for _, key := range toolKeys {
		if key == "cluster-access" {
			env = append(env,
				corev1.EnvVar{Name: "EXECUTOR_ADDR", Value: cluster.ExecutorAddr},
				corev1.EnvVar{Name: "THOT_AUTH_TOKEN", Value: cluster.AuthToken},
			)
		}
	}

	if len(toolKeys) > 0 {
		volume = &corev1.Volume{Name: toolsVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
		mount = &corev1.VolumeMount{Name: toolsVolumeName, MountPath: toolsMountPath}
		env = append(env, corev1.EnvVar{
			Name: "PATH",
			// Just /opt/tools/bin now: cluster-access's kubectl shim is the
			// only thing staged here. /opt/tools/go/bin and a GOROOT override
			// went with the toolchain ingredients (docs/adr/0048 §6) — and
			// they were actively harmful while they lasted, since the go entry
			// came first and shadowed the image's own Go.
			Value: "/opt/tools/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		})
	}

	for _, si := range serviceIngredients {
		if si.ScopeMode != ScopeModePodScoped {
			continue // minted separately by the caller, see doc comment above
		}
		def, ok := catalog.Services[si.Key]
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("unknown service ingredient %q", si.Key)
		}
		sidecar, sidecarEnv := podScopedServiceSidecar(si.Key, def)
		initContainers = append(initContainers, sidecar)
		env = append(env, sidecarEnv...)
	}

	return initContainers, env, volume, mount, nil
}

// podScopedServiceSidecarUser/Password/DB are fixed, non-sensitive defaults
// — a pod-scoped instance is localhost-only within one ephemeral pod, never
// exposed via a Service, so there's no credential-secrecy requirement the
// way there is for a shared instance's admin password (credentials.go).
const podScopedServiceCred = "app"

// podScopedServiceSidecar mirrors CreateWorkerPod's own native-sidecar
// pattern (RestartPolicy Always + StartupProbe blocks the main container
// from starting until ready) — the same mechanism, applied to a service
// ingredient instead of the worker's MCP sidecar.
func podScopedServiceSidecar(key string, def catalog.ServiceDef) (corev1.Container, []corev1.EnvVar) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	image := ""
	var envVars []corev1.EnvVar
	var probeCmd []string
	var connEnv []corev1.EnvVar

	switch key {
	case "postgres":
		image = "postgres:16-alpine" // ponytail: fixed for pod-scoped mode; shared-instance mode uses the configurable PostgresImage
		envVars = []corev1.EnvVar{
			{Name: "POSTGRES_USER", Value: podScopedServiceCred},
			{Name: "POSTGRES_PASSWORD", Value: podScopedServiceCred},
			{Name: "POSTGRES_DB", Value: podScopedServiceCred},
		}
		probeCmd = []string{"pg_isready", "-U", podScopedServiceCred}
		connEnv = []corev1.EnvVar{{
			Name:  def.EnvVarName,
			Value: fmt.Sprintf("postgresql://%s:%s@localhost:%d/%s?sslmode=disable", podScopedServiceCred, podScopedServiceCred, def.Port, podScopedServiceCred),
		}}
	case "redis":
		image = "redis:7-alpine"
		probeCmd = []string{"redis-cli", "-a", podScopedServiceCred, "ping"}
		connEnv = []corev1.EnvVar{{
			Name:  def.EnvVarName,
			Value: fmt.Sprintf("redis://:%s@localhost:%d", podScopedServiceCred, def.Port),
		}}
	}
	if key == "redis" {
		envVars = nil // set via --requirepass instead, mirrors shared_instance.go's own redis handling
	}

	container := corev1.Container{
		Name:          key,
		Image:         image,
		RestartPolicy: &restartAlways,
		Env:           envVars,
		Ports:         []corev1.ContainerPort{{ContainerPort: def.Port}},
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: probeCmd}},
			PeriodSeconds:    2,
			FailureThreshold: 60,
		},
	}
	if key == "redis" {
		container.Command = []string{"redis-server", "--requirepass", podScopedServiceCred}
	}
	return container, connEnv
}
