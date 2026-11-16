package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The SIDECAR is what talks to core, and CoreService now rejects a caller with
// no lease. The worker container has carried LEASE_ID for a long time and that
// copy authenticates nothing — it never opens a gRPC connection.
//
// Getting this wrong is not subtle at runtime (the sidecar refuses to start),
// but it is completely invisible here: the manifest is valid without it, the
// fake clientset never runs a container, and every other test still passes.
func TestCreateWorkerPod_SidecarCarriesTheSessionLease(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-abc"}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	sidecar := job.Spec.Template.Spec.InitContainers[1]
	// ADR-0086 §6: env holds a path; the lease is in the pod's Secret.
	var got string
	for _, e := range sidecar.Env {
		if e.Name == "LEASE_ID" {
			t.Error("sidecar has LEASE_ID in env")
		}
		if e.Name == "LEASE_ID_FILE" {
			got = e.Value
		}
	}
	if got != "/run/fleet/cred/lease" {
		t.Errorf("sidecar LEASE_ID_FILE = %q — without it every CoreService call this pod makes is rejected", got)
	}
	sec, err := c.Core.CoreV1().Secrets("agent-fleet").Get(ctx, CredSecretName("task-1"), metav1.GetOptions{})
	if err != nil || sec.StringData["lease"] != "lease-abc" {
		t.Errorf("credential secret %+v, %v", sec, err)
	}

	// And the pair must match: a sidecar authenticating as one session while
	// the worker believes it is another would pass every per-container check
	// and fail only on the wire.
	var sidecarSession string
	for _, e := range sidecar.Env {
		if e.Name == "SESSION_ID" {
			sidecarSession = e.Value
		}
	}
	if sidecarSession != "task-1" {
		t.Errorf("sidecar SESSION_ID = %q, want %q", sidecarSession, "task-1")
	}
}
