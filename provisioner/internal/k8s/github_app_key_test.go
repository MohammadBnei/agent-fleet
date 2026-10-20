package k8s

import (
	"os"
	"strings"
	"testing"
)

// docs/adr/0075: the provisioner may mount any Secret in its namespace into a
// pod it creates, so the App key's Secret must never appear in a worker pod.
// Definition pods mount no Secret at all (TestDefinitionPod_NoRealSecret);
// this pins the repo-session shape.
func TestRepoSessionPod_NeverReferencesTheAppKey(t *testing.T) {
	b, err := os.ReadFile("testdata/worker_job_repo_session.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"provisioner-github-app", "GITHUB_APP_"} {
		if strings.Contains(string(b), ref) {
			t.Errorf("repo-session pod references %s", ref)
		}
	}
}
