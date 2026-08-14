package grpcserver

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// markTerminating stamps a deletionTimestamp the way the API server does on
// a graceful delete — the object stays Get-able, and its phase stays
// Running, for the whole terminationGracePeriod. The fake clientset deletes
// immediately, so the state has to be forged to reproduce the window.
func markTerminating(t *testing.T, k8sc *k8s.Client, taskID string) {
	t.Helper()
	ctx := context.Background()
	pod, err := k8sc.Core.CoreV1().Pods("agent-fleet").Get(ctx, k8s.ResourceName(taskID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now
	if _, err := k8sc.Core.CoreV1().Pods("agent-fleet").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark terminating: %v", err)
	}
}

// TestGetE2ESessionStatus_TerminatingIsNotRunning: kubelet reports Running
// for a pod's whole grace period, so status derived from phase alone tells
// the dashboard and the agent that a pod seconds from disappearing is a
// healthy session.
func TestGetE2ESessionStatus_TerminatingIsNotRunning(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{SessionId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("CreateE2ESession: %v", err)
	}
	markTerminating(t, k8sc, "t1")

	resp, err := s.GetE2ESessionStatus(ctx, &agentfleetv1.GetE2ESessionStatusRequest{SessionId: "t1"})
	if err != nil {
		t.Fatalf("GetE2ESessionStatus: %v", err)
	}
	if resp.GetStatus() != "terminating" {
		t.Errorf("status = %q, want %q — a dying pod must not report as a live session", resp.GetStatus(), "terminating")
	}
	if resp.GetPodPhase() != "Terminating" {
		t.Errorf("podPhase = %q, want %q", resp.GetPodPhase(), "Terminating")
	}
}

// TestCreateE2ESession_TerminatingPodIsNotAnExistingSession is the exact
// reported bug: kill_env followed by request_e2e_env — the fleet's single
// most common agent sequence — found the dying pod still present, took the
// idempotent early-return path, and handed back a preview URL for a pod
// that was about to vanish. The caller saw success and got nothing.
func TestCreateE2ESession_TerminatingPodIsNotAnExistingSession(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{SessionId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("first CreateE2ESession: %v", err)
	}
	markTerminating(t, k8sc, "t1")

	// The pod is gone from under the wait loop shortly after the call
	// starts — exactly what the grace period expiring looks like.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = k8sc.Core.CoreV1().Pods("agent-fleet").Delete(ctx, k8s.ResourceName("t1"), metav1.DeleteOptions{})
	}()

	resp, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{SessionId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev"})
	if err != nil {
		t.Fatalf("CreateE2ESession after terminate: %v", err)
	}
	if resp.GetPreviewUrl() == "" {
		t.Fatal("expected a preview URL for the freshly created session")
	}

	pod, err := k8sc.Core.CoreV1().Pods("agent-fleet").Get(ctx, k8s.ResourceName("t1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected a new e2e pod to exist: %v", err)
	}
	if pod.DeletionTimestamp != nil {
		t.Error("the returned session is still the terminating pod — a new one was never created")
	}
}

// TestCreateE2ESession_TerminatingPodThatNeverGoesAwayFailsLoudly: if the
// old pod is wedged, the caller gets a real error naming the cause, not a
// URL to nothing. Silence was the whole complaint.
func TestCreateE2ESession_TerminatingPodThatNeverGoesAwayFailsLoudly(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{SessionId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("first CreateE2ESession: %v", err)
	}
	markTerminating(t, k8sc, "t1")

	// Cancelling stands in for the 2-minute timeout without waiting for it.
	cancelCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	if _, err := s.CreateE2ESession(cancelCtx, &agentfleetv1.CreateE2ESessionRequest{SessionId: "t1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err == nil {
		t.Fatal("expected an error when the previous pod never finishes terminating, got nil")
	}
}
