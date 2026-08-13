package grpcserver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// markPhase forges a terminal pod phase. The fake clientset never runs a
// kubelet, so nothing ever moves a pod out of "" on its own — the same reason
// markTerminating has to stamp a deletionTimestamp by hand.
func markPhase(t *testing.T, k8sc *k8s.Client, taskID string, phase corev1.PodPhase) {
	t.Helper()
	ctx := context.Background()
	pod, err := k8sc.Core.CoreV1().Pods("agent-fleet").Get(ctx, k8s.ResourceName(taskID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1, Message: "the app command exited",
		}},
	}}
	if _, err := k8sc.Core.CoreV1().Pods("agent-fleet").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark phase: %v", err)
	}
}

// TestCreateE2ESession_DeadPodIsNotAnExistingSession is docs/adr/0044's
// permanent-dead-end bug. A Failed pod still reports exists=true and
// non-Terminating forever, so the idempotent early-return handed back the
// corpse's preview URL on every subsequent call — and run_command's retry
// then failed against an unreachable pod for the rest of the session, with
// nothing GC-ing it. Only kill_env could break the loop.
func TestCreateE2ESession_DeadPodIsNotAnExistingSession(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			s, k8sc, _ := newTestServer(t)
			ctx := context.Background()

			if _, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
				TaskId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev",
			}); err != nil {
				t.Fatalf("first CreateE2ESession: %v", err)
			}
			markPhase(t, k8sc, "t1", phase)

			resp, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
				TaskId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev",
			})
			if err != nil {
				t.Fatalf("CreateE2ESession over a dead pod: %v", err)
			}
			if resp.GetPreviewUrl() == "" {
				t.Fatal("expected a preview URL for the freshly created session")
			}

			pod, err := k8sc.Core.CoreV1().Pods("agent-fleet").Get(ctx, k8s.ResourceName("t1"), metav1.GetOptions{})
			if err != nil {
				t.Fatalf("expected a replacement e2e pod to exist: %v", err)
			}
			if pod.Status.Phase == phase {
				t.Errorf("still the %s corpse — it was never replaced", phase)
			}
		})
	}
}

// A sandbox-only pod (no app command) must be creatable through the RPC too,
// not just through k8s.CreatePod — this is the path run_command takes for a
// repo with no "e2e" profile, which used to fail outright (docs/adr/0044).
func TestCreateE2ESession_NoStartCmdStillCreatesASandbox(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()

	resp, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
		TaskId: "t1", Repo: "agent-fleet",
	})
	if err != nil {
		t.Fatalf("CreateE2ESession with no start command: %v", err)
	}
	if resp.GetStatus() != "requested" {
		t.Errorf("status = %q, want %q — the pod is Pending, not running", resp.GetStatus(), "requested")
	}
	if _, err := k8sc.Core.CoreV1().Pods("agent-fleet").Get(ctx, k8s.ResourceName("t1"), metav1.GetOptions{}); err != nil {
		t.Fatalf("expected a sandbox pod to exist: %v", err)
	}
}
