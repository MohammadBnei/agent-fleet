package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
)

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
func buildIngredients(toolKeys []string, serviceIngredients []ServiceIngredientRef) (initContainers []corev1.Container, env []corev1.EnvVar, volume *corev1.Volume, mount *corev1.VolumeMount, err error) {
	hasGo := false
	for _, key := range toolKeys {
		def, ok := catalog.Tools[key]
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("unknown tool ingredient %q", key)
		}
		if key == "go-toolchain" {
			hasGo = true
		}
		initContainers = append(initContainers, corev1.Container{
			Name:         "tool-" + key,
			Image:        def.CopyImage,
			Command:      def.CopyCmd,
			VolumeMounts: []corev1.VolumeMount{{Name: toolsVolumeName, MountPath: toolsMountPath}},
		})
	}
	if len(toolKeys) > 0 {
		volume = &corev1.Volume{Name: toolsVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
		mount = &corev1.VolumeMount{Name: toolsVolumeName, MountPath: toolsMountPath}
		env = append(env, corev1.EnvVar{
			Name: "PATH",
			// /opt/tools/bin covers bun/golangci-lint/buf's single-binary
			// copies; /opt/tools/go/bin covers the go-toolchain's own bin
			// dir — harmless to include even when go-toolchain isn't
			// requested, PATH entries for a nonexistent dir are a no-op.
			Value: "/opt/tools/bin:/opt/tools/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		})
		if hasGo {
			env = append(env, corev1.EnvVar{Name: "GOROOT", Value: "/opt/tools/go"})
		}
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
