package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// docs/adr/0088: a definition pod gets its own key (name upper-cased, dashes to
// underscores), never the console's; no key provisioned leaves the Secret
// without GATEWAY_KEY so the sidecar answers 503. The key goes to the Secret
// only, never the Job.
func TestCreateWorkerPod_GatewayKey(t *testing.T) {
	for _, tc := range []struct {
		name, def, want string
		shared          bool
	}{
		{"definition", "my-def", "gw-def-key", false},
		{"own key wins over the shared one", "my-def", "gw-def-key", true},
		{"definition without a key", "other", "", false},
		// A Claude-model definition pod: core does not ask for the shared key.
		{"no shared key unless the create asks", "other", "", false},
		{"gateway definition without a key takes the shared one", "other", "gw-shared-key", true},
	} {
		c := newTestClient()
		c.GatewayURL, c.GatewayKeys = "http://gw.test:8080", map[string]string{"CONSOLE": "gw-console-key", "MY_DEF": "gw-def-key", "DEFINITIONS": "gw-shared-key"}
		ctx := context.Background()
		if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
			SessionID: "task-1", Repo: "r", RepoURL: "https://github.com/o/r.git", LeaseID: "l", SharedGatewayKey: tc.shared,
			Definition: &DefinitionProfile{Name: tc.def, ProxyURL: "http://p", DeadlineSeconds: 60},
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		sec, err := c.Core.CoreV1().Secrets("agent-fleet").Get(ctx, CredSecretName("task-1"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := sec.StringData["GATEWAY_KEY"]; got != tc.want {
			t.Errorf("%s: GATEWAY_KEY %q, want %q", tc.name, got, tc.want)
		}
		job, _ := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
		var sidecarKeyMounted bool
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Name == "cred-sidecar" {
				for _, it := range v.Secret.Items {
					sidecarKeyMounted = sidecarKeyMounted || it.Key == "GATEWAY_KEY"
				}
			}
			if v.Name == "cred-worker" {
				t.Errorf("%s: worker has a cred volume", tc.name)
			}
		}
		if sidecarKeyMounted != (tc.want != "") {
			t.Errorf("%s: sidecar mounts the key = %v", tc.name, sidecarKeyMounted)
		}
	}
}

// The console key is the repo sessions' and never reaches a definition pod,
// even one that asks for the shared key while none exists.
func TestGatewayKey_DefinitionNeverGetsConsoleKey(t *testing.T) {
	c := newTestClient()
	c.GatewayKeys = map[string]string{"CONSOLE": "gw-console-key"}
	got := c.gatewayKey(WorkerPodSpec{Definition: &DefinitionProfile{Name: "x"}, SharedGatewayKey: true})
	if got != "" {
		t.Fatalf("definition got %q", got)
	}
	if k := c.gatewayKey(WorkerPodSpec{}); k != "gw-console-key" {
		t.Fatalf("repo session got %q", k)
	}
}

func TestGatewayKeyNames(t *testing.T) {
	c := newTestClient()
	c.GatewayKeys = map[string]string{"MY_DEF": "secret-b", "CONSOLE": "secret-a"}
	got := c.GatewayKeyNames()
	if len(got) != 2 || got[0] != "CONSOLE" || got[1] != "MY_DEF" {
		t.Fatalf("names = %v", got)
	}
}
