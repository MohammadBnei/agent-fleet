package k8s

import (
	"context"
	"log/slog"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The per-task sandbox NetworkPolicy (docs/adr/0045).
//
// Before direct dial, the only thing that could reach a sandbox's MCP ports
// was the provisioner, and the static `e2e-runner` policy in
// k8s/provisioner/networkpolicy.yaml said exactly that. Once a sidecar dials
// its sandbox itself, "any worker pod" would be reachable from any other
// task's sandbox unless something narrows it — and every worker pod already
// carries the `app.kubernetes.io/part-of: agent-fleet` label that policy's
// rule 2 admits to port 3000.
//
// So authorization here is structural rather than a token the sandbox would
// have to hold and verify. That is not just principle 7's ranking: docs/adr/
// 0039 rests on the sandbox holding *no* fleet credentials, which is what
// lets run_command stay un-prompted while Bash is canUseTool-gated. Handing
// it a bearer token to check would reopen that decision, and would drag the
// e2e-runner image into the deploy-skew matrix for good measure.
//
// NetworkPolicies are ADDITIVE — the union of every policy selecting a pod.
// This one therefore *adds* the MCP ports for one specific worker without
// touching the static policy's broader rules. The corollary is a hazard: a
// later broad rule added to the static policy silently widens every per-task
// policy too, because there is no deny to override.
//
// Enforcement is the CNI's. Cilium does this on ukubi-cluster; kind's default
// CNI does NOT, so /kind-local can prove the positive path and nothing at all
// about isolation. The negative test is real-cluster-only.
func networkPolicyName(taskID string) string { return ResourceName(taskID) + "-mcp" }

// CreateNetworkPolicy admits exactly one worker pod — the one belonging to
// this task — to this task's sandbox MCP ports.
//
// Create-if-absent, like CreateService/CreateMiddleware/CreateIngressRoute:
// docs/adr/0044's recreate path deletes only the Pod, so a surviving policy
// must not turn the next CreateE2ESession into an error.
func (c *Client) CreateNetworkPolicy(ctx context.Context, taskID string) error {
	name := networkPolicyName(taskID)
	playwrightPort := intstr.FromInt32(PlaywrightPort)
	execPort := intstr.FromInt32(ExecPort)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: Labels(taskID)},
		Spec: networkingv1.NetworkPolicySpec{
			// Selects the sandbox pods of this task only.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				ComponentLabel: ComponentE2eRun,
				TaskIDLabel:    taskID,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					// Both labels, deliberately. ComponentLabel alone would
					// admit every worker in the fleet; TaskIDLabel alone would
					// admit this task's own sandbox pod, which carries the
					// same task id.
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						ComponentLabel: ComponentWorker,
						TaskIDLabel:    taskID,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Port: &playwrightPort},
					{Port: &execPort},
				},
			}},
		},
	}
	_, err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err = ignoreAlreadyExists(err); err != nil {
		slog.Error("k8s CreateNetworkPolicy", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s CreateNetworkPolicy", "taskId", taskID, "name", name)
	return nil
}

// DeleteNetworkPolicy is best-effort on teardown, mirroring how the other
// per-task objects are cleaned up: a leftover policy selecting a pod that no
// longer exists denies nothing and blocks nothing, so failing a teardown over
// it would be worse than leaving it.
func (c *Client) DeleteNetworkPolicy(ctx context.Context, taskID string) error {
	name := networkPolicyName(taskID)
	err := c.Core.NetworkingV1().NetworkPolicies(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err = ignoreNotFound(err); err != nil {
		slog.Error("k8s DeleteNetworkPolicy", "taskId", taskID, "error", err)
		return err
	}
	return nil
}
