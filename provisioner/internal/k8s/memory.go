package k8s

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// MemoryProfile gives a long-lived agent's pod its memory repos (docs/adr/0086
// §5). The sidecar is the single writer; the worker gets the memory repo
// read-only, only when its set holds B, and never the quarantine repo.
type MemoryProfile struct {
	Agent string // the agent's name
	Set   string // the session's stored set, e.g. "AB" or "BC"
}

const memoryDir = "/memory"

var memorySetRe = regexp.MustCompile(`^(A|AB|AC|ABC|B|BC|C)$`)

var memoryAgentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// applyMemory wires m into the pod: env and rw mounts for the sidecar, a
// read-only memory mount for the worker when the set holds B.
func applyMemory(ps *corev1.PodSpec, m MemoryProfile) error {
	if !memoryAgentRe.MatchString(m.Agent) {
		return fmt.Errorf("memory: bad agent name %q", m.Agent)
	}
	mem, quar := "agent-"+m.Agent+"-memory", "agent-"+m.Agent+"-quarantine"
	vm := func(dir string, ro bool) corev1.VolumeMount {
		return corev1.VolumeMount{Name: "shared", MountPath: memoryDir + "/" + dir, SubPath: "claude-home/" + dir, ReadOnly: ro}
	}
	if !memorySetRe.MatchString(m.Set) {
		return fmt.Errorf("memory: bad trust set %q", m.Set)
	}
	// The sidecar mounts only the repo this set may write: quarantine when it
	// holds A, memory when it holds C without A, none otherwise.
	var write []corev1.VolumeMount
	switch {
	case strings.Contains(m.Set, "A"):
		write = append(write, vm(quar, false))
	case strings.Contains(m.Set, "C"):
		write = append(write, vm(mem, false))
	}
	found := false
	for i := range ps.InitContainers {
		if c := &ps.InitContainers[i]; c.Name == "sidecar" {
			found = true
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "FLEET_AGENT_NAME", Value: m.Agent},
				corev1.EnvVar{Name: "FLEET_TRUST_SET", Value: m.Set},
				corev1.EnvVar{Name: "FLEET_MEMORY_ROOT", Value: memoryDir})
			c.VolumeMounts = append(c.VolumeMounts, write...)
		}
	}
	if !found {
		return fmt.Errorf("memory: pod has no sidecar init container")
	}
	if strings.Contains(m.Set, "B") {
		w := &ps.Containers[0]
		w.VolumeMounts = append(w.VolumeMounts, vm(mem, true))
		// The repo is root-owned and the worker is uid 1000: without safe.directory every git read of it
		// fails "dubious ownership", which hides the per-commit provenance.
		dir := memoryDir + "/" + mem
		n := 0
		for i := range w.Env {
			if w.Env[i].Name == "GIT_CONFIG_COUNT" {
				n, _ = strconv.Atoi(w.Env[i].Value)
				w.Env[i].Value = strconv.Itoa(n + 1)
				break
			}
		}
		if n == 0 {
			w.Env = append(w.Env, corev1.EnvVar{Name: "GIT_CONFIG_COUNT", Value: "1"})
		}
		w.Env = append(w.Env,
			corev1.EnvVar{Name: "FLEET_MEMORY_DIR", Value: dir},
			corev1.EnvVar{Name: fmt.Sprintf("GIT_CONFIG_KEY_%d", n), Value: "safe.directory"},
			corev1.EnvVar{Name: fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), Value: dir})
	}
	return nil
}
