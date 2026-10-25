package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// docs/adr/0080 §2: with the local stack's rewrite set, both vault profiles'
// workers send https://github.com/ to the forge, as the session (the forge
// records the user name as who pushed), after the entries already there.
func TestCreateWorkerPod_GitURLRewrite(t *testing.T) {
	repo := definitionSpec()
	repo.SessionID, repo.Definition = "task-2", nil
	repo.RepoVault = &RepoVault{Vault: "owner-o", ProxyURL: "http://tok@vault:1", DeadlineSeconds: 60, VaultLabel: "task-2.1"}
	for name, spec := range map[string]WorkerPodSpec{"definition": definitionSpec(), "repo": repo} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
			c := newTestClient()
			c.GitURLRewrite = "http://host.docker.internal:7480/"
			ctx := context.Background()
			if err := c.CreateWorkerPod(ctx, spec); err != nil {
				t.Fatalf("CreateWorkerPod: %v", err)
			}
			job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName(spec.SessionID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			worker := container(t, job, "worker")
			for k, want := range map[string]string{
				"GIT_CONFIG_COUNT":   "2",
				"GIT_CONFIG_KEY_0":   "safe.directory",
				"GIT_CONFIG_KEY_1":   "url.http://" + spec.SessionID + ":x@host.docker.internal:7480/.insteadOf",
				"GIT_CONFIG_VALUE_1": "https://github.com/",
			} {
				if got, _ := findEnv(worker.Env, k); got != want {
					t.Errorf("worker %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}
