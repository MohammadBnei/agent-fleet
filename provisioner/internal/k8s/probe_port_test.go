package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// kubelet dials a probe at the POD IP, never at 127.0.0.1. The sidecar's mcp
// and local-api listeners are loopback-bound on purpose — everything they serve
// acts under the session's own authority — so SidecarHealthPort is the only one
// reachable from outside the container.
//
// Pointing the probe at either loopback port is silent and expensive: the
// sidecar is healthy, the probe fails all 60 attempts, and two minutes later
// the pod dies with nothing in any log saying why. Nothing else ties these two
// numbers together — the manifest is valid either way, and the fake clientset
// never executes a probe, so this assertion is the only thing standing between
// a one-word edit and a fleet that cannot start a session.
func TestCreateWorkerPod_StartupProbeTargetsThePortTheSidecarActuallyBinds(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	sidecar := job.Spec.Template.Spec.InitContainers[1]

	probe := sidecar.StartupProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatalf("sidecar must keep an HTTP startup probe, got %+v", probe)
	}
	if got := int32(probe.HTTPGet.Port.IntValue()); got != SidecarHealthPort {
		t.Errorf("StartupProbe port = %d, want SidecarHealthPort (%d) — %d and %d bind 127.0.0.1 and kubelet cannot reach them",
			got, SidecarHealthPort, SidecarMCPPort, SidecarAPIPort)
	}

	// The sidecar takes its health port from this env var. Probe and listener
	// can only agree if both derive from the same constant; without it the
	// sidecar silently falls back to its own hardcoded default.
	var healthEnv string
	for _, e := range sidecar.Env {
		if e.Name == "HEALTH_PORT" {
			healthEnv = e.Value
		}
	}
	if healthEnv == "" {
		t.Error("sidecar has no HEALTH_PORT env — its listener could drift from the probe with nothing failing")
	}

	// Only the health port belongs in ContainerPorts. Declaring the loopback
	// ones reads as though they were reachable, which is the belief this whole
	// change exists to correct.
	for _, p := range sidecar.Ports {
		if p.ContainerPort != SidecarHealthPort {
			t.Errorf("sidecar declares ContainerPort %d (%q), but only SidecarHealthPort binds beyond loopback", p.ContainerPort, p.Name)
		}
	}
}
