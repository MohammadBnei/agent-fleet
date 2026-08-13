package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The per-task policy is the only thing standing between one task's worker
// and another task's sandbox once the sidecar dials directly (docs/adr/0045).
// Its two failure modes are asymmetric: too narrow locks a worker out of its
// own sandbox and fails loudly on the next run_command, while too wide leaves
// every sandbox open to every task and fails silently, forever.
//
// These tests only pin manifest *shape*. Whether the CNI enforces any of it
// is untestable here and untestable under /kind-local, whose default CNI
// ignores NetworkPolicy entirely — that check is real-cluster-only.
func policyFor(t *testing.T, taskID string) (*Client, context.Context) {
	t.Helper()
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateNetworkPolicy(ctx, taskID); err != nil {
		t.Fatalf("CreateNetworkPolicy: %v", err)
	}
	return c, ctx
}

func TestCreateNetworkPolicy_AdmitsOnlyThisTasksWorker(t *testing.T) {
	c, ctx := policyFor(t, "task-1")
	np, err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Get(ctx, networkPolicyName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}

	// Selects this task's sandbox, and nothing else.
	sel := np.Spec.PodSelector.MatchLabels
	if sel[ComponentLabel] != ComponentE2eRun || sel[TaskIDLabel] != "task-1" {
		t.Errorf("policy selects %+v, want this task's e2e-runner pods", sel)
	}

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected exactly 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 || rule.From[0].PodSelector == nil {
		t.Fatalf("expected exactly one pod-selector peer, got %+v", rule.From)
	}
	from := rule.From[0].PodSelector.MatchLabels

	// Both labels are load-bearing. Component alone admits every worker in the
	// fleet; task-id alone admits this task's own sandbox pod, which carries
	// the same task id.
	if from[ComponentLabel] != ComponentWorker {
		t.Errorf("peer component = %q, want %q — without it any pod with this task id is admitted", from[ComponentLabel], ComponentWorker)
	}
	if from[TaskIDLabel] != "task-1" {
		t.Errorf("peer task-id = %q, want task-1 — without it EVERY task's worker is admitted, which is the whole risk", from[TaskIDLabel])
	}

	ports := map[int32]bool{}
	for _, p := range rule.Ports {
		if p.Port != nil {
			ports[p.Port.IntVal] = true
		}
	}
	if !ports[PlaywrightPort] || !ports[ExecPort] {
		t.Errorf("admitted ports %v, want both %d and %d", ports, PlaywrightPort, ExecPort)
	}
	// App and code-server must NOT be here: code-server is a full IDE the
	// static policy deliberately denies workers, and widening this rule would
	// hand it back through the side door.
	if ports[CodeServerPort] || ports[AppPort] {
		t.Errorf("admitted ports %v — this policy covers MCP only; app/code-server access belongs to the static policy", ports)
	}
}

// The peer selector matches the labels WorkerLabels actually sets. Two
// independent maps that must agree, and a mismatch denies silently — the
// sidecar just times out against its own sandbox.
func TestCreateNetworkPolicy_PeerMatchesRealWorkerLabels(t *testing.T) {
	c, ctx := policyFor(t, "task-1")
	np, err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Get(ctx, networkPolicyName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	worker := WorkerLabels("task-1", "some-repo")
	for k, v := range np.Spec.Ingress[0].From[0].PodSelector.MatchLabels {
		if worker[k] != v {
			t.Errorf("policy admits %s=%q, but a real worker pod carries %s=%q — the policy would deny its own task", k, v, k, worker[k])
		}
	}
}

// A worker for a DIFFERENT task must not satisfy the selector. Stated as its
// own test because it is the property the whole PR exists for, and it is the
// one that fails silently if the task-id label is ever dropped from the peer.
func TestCreateNetworkPolicy_RejectsAnotherTasksWorker(t *testing.T) {
	c, ctx := policyFor(t, "task-1")
	np, err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Get(ctx, networkPolicyName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	other := WorkerLabels("task-2", "some-repo")
	admits := true
	for k, v := range np.Spec.Ingress[0].From[0].PodSelector.MatchLabels {
		if other[k] != v {
			admits = false
			break
		}
	}
	if admits {
		t.Error("task-2's worker satisfies task-1's sandbox policy — cross-task sandbox access is exactly what this must prevent")
	}
}

// Create-if-absent: docs/adr/0044's recreate path deletes only the Pod, so a
// surviving policy must not turn the next CreateE2ESession into an error.
func TestCreateNetworkPolicy_IsIdempotent(t *testing.T) {
	c, ctx := policyFor(t, "task-1")
	if err := c.CreateNetworkPolicy(ctx, "task-1"); err != nil {
		t.Fatalf("second CreateNetworkPolicy must be a no-op, got: %v", err)
	}
}

// DeleteAll is the single funnel every teardown path uses, so the policy has
// to go from there rather than from each caller.
func TestDeleteAll_RemovesTheNetworkPolicy(t *testing.T) {
	c, ctx := policyFor(t, "task-1")
	if err := c.DeleteAll(ctx, "task-1"); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	_, err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Get(ctx, networkPolicyName("task-1"), metav1.GetOptions{})
	if err == nil {
		t.Error("the per-task policy outlived its sandbox — teardown must remove it")
	}
}

// Deleting a policy that was never created is a no-op, not an error: teardown
// runs against sessions created before this code existed.
func TestDeleteNetworkPolicy_ToleratesMissing(t *testing.T) {
	c := newTestClient()
	if err := c.DeleteNetworkPolicy(context.Background(), "never-created"); err != nil {
		t.Errorf("deleting an absent policy must be a no-op, got: %v", err)
	}
}
