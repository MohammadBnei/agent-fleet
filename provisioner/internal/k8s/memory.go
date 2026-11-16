package k8s

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// MemoryProfile gives a long-lived agent's pod its memory repos (docs/adr/0086
// §5). The sidecar is the single writer of both; the worker gets the memory repo
// read-only, only when its set holds B, and never the quarantine repo.
type MemoryProfile struct {
	Agent string // the agent's name
	Set   string // the session's stored set, e.g. "AB" or "BC"
}

const memoryDir = "/memory"

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
	for i := range ps.InitContainers {
		if c := &ps.InitContainers[i]; c.Name == "sidecar" {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "FLEET_AGENT_NAME", Value: m.Agent},
				corev1.EnvVar{Name: "FLEET_TRUST_SET", Value: m.Set},
				corev1.EnvVar{Name: "FLEET_MEMORY_ROOT", Value: memoryDir})
			c.VolumeMounts = append(c.VolumeMounts, vm(mem, false), vm(quar, false))
		}
	}
	if strings.Contains(m.Set, "B") {
		ps.Containers[0].VolumeMounts = append(ps.Containers[0].VolumeMounts, vm(mem, true))
	}
	return nil
}
