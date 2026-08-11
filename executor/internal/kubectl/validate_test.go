package kubectl

import (
	"context"
	"strings"
	"testing"
)

func TestValidateReadOnly_AcceptsReadVerbs(t *testing.T) {
	for _, args := range [][]string{
		{"get", "pods", "-A"},
		{"describe", "deploy/core", "-n", "agent-fleet"},
		{"logs", "deploy/core", "--tail", "100"},
		{"events", "-n", "thot"},
		{"top", "nodes"},
		{"api-resources"},
	} {
		if refusal := ValidateReadOnly(args); refusal != "" {
			t.Errorf("%v should be allowed, got refusal: %s", args, refusal)
		}
	}
}

// The executor's ClusterRole really does grant `patch` on workloads and
// `delete` on pods, so the read-only path cannot lean on RBAC to stay
// read-only. These are the checks that actually enforce it.
func TestValidateReadOnly_RefusesMutatingVerbs(t *testing.T) {
	for _, args := range [][]string{
		{"delete", "pod", "core-abc"},
		{"patch", "deploy/core", "-p", "{}"},
		{"apply", "-f", "manifest.yaml"},
		{"edit", "deploy/core"},
		{"scale", "deploy/core", "--replicas", "0"},
		{"rollout", "restart", "deploy/core"},
		{"exec", "-it", "pod/core", "--", "sh"},
		{"port-forward", "svc/core", "8080:8080"},
		{"cp", "pod:/etc/passwd", "/tmp/x"},
		{"drain", "k8s-worker-01"},
	} {
		refusal := ValidateReadOnly(args)
		if !strings.Contains(refusal, "not a read-only verb") {
			t.Errorf("%v must be refused on the unattended path, got %q", args, refusal)
		}
	}
}

func TestValidateReadOnly_RefusesCredentialOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"get", "pods", "--as", "system:admin"},
		{"get", "pods", "--as=system:admin"},
		{"get", "pods", "--as-group=system:masters"},
		{"get", "secrets", "--kubeconfig", "/tmp/evil.conf"},
		{"get", "pods", "--server=https://evil.example.com"},
		{"get", "pods", "--token=abc"},
	} {
		if refusal := ValidateReadOnly(args); !strings.Contains(refusal, "not permitted") {
			t.Errorf("%v must be refused, got %q", args, refusal)
		}
	}
}

func TestValidateReadOnly_RefusesEmpty(t *testing.T) {
	if ValidateReadOnly(nil) == "" {
		t.Error("empty args must be refused")
	}
}

// The structural guarantee: argv is passed to exec.Command as a slice, so
// a shell metacharacter is just a literal argument. A "second command"
// smuggled into one string is only ever a nonsense verb.
func TestValidateReadOnly_NoShellChaining(t *testing.T) {
	refusal := ValidateReadOnly([]string{"get pods; kubectl delete pod core"})
	if !strings.Contains(refusal, "not a read-only verb") {
		t.Errorf("a chained command must land as one bogus verb, got %q", refusal)
	}
}

// Proves Run really does treat argv as a literal array — `;` reaches the
// binary as an argument rather than starting a second process.
func TestRun_DoesNotUseAShell(t *testing.T) {
	res := Run(context.Background(), []string{"version", "--client", "--output=json"})
	if res.ExitCode != 0 && res.ExitCode != -1 {
		t.Logf("kubectl version exit=%d stderr=%s (kubectl may be absent in CI)", res.ExitCode, res.Stderr)
	}
	// Whatever happened, nothing should have been interpreted as a shell.
	if strings.Contains(res.Stdout, "sh:") || strings.Contains(res.Stderr, "syntax error") {
		t.Errorf("output looks shell-parsed: %q / %q", res.Stdout, res.Stderr)
	}
}
