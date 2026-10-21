package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// A repo session's Job, whole, so any drift in the ordinary pod shows up here
// as a diff, not as a review miss. Regenerate with -update only for a change
// that means to alter repo-session pods (last: the agent-fleet.dev/version
// label and FLEET_VERSION env). GH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN are set
// to prove neither reaches the pod.
func TestCreateWorkerPod_RepoSessionGolden(t *testing.T) {
	for k, v := range map[string]string{
		"GH_TOKEN": "gh-token", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-token", "CLAUDE_MODEL": "model", "MAX_TURNS": "7",
	} {
		t.Setenv(k, v)
	}
	c := newTestClient()
	c.Version = "6.22.1"
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
		SessionID: "task-1", Repo: "dream-analyst", RepoURL: "https://github.com/o/dream-analyst.git", BaseBranch: "dev",
		LeaseID: "lease-1", ResumeID: "resume-1", ResumeFromSeq: 42,
		RepoVault: &RepoVault{
			Vault: "owner-o", ProxyURL: "http://tok:owner-o@agent-vault.agent-vault.svc.cluster.local:14322",
			DeadlineSeconds: 43200, VaultLabel: "task-1.1", GitAuthorName: "ukubi", GitAuthorEmail: "ukubi@bnei.dev",
		},
	}); err != nil {
		t.Fatal(err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/worker_job_repo_session.golden.json"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("repo-session Job drifted from %s:\n%s", path, got)
	}
}
