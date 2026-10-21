package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The release that built a pod rides on the Job and pod as a label; a version
// that is not a valid label value drops the label rather than failing the
// create. It is deliberately NOT container env: it would leak into the agent's
// Bash, and a session running core or the provisioner from source would pick
// up a release instead of "dev" (which moves the definitions ref).
func TestCreateWorkerPod_StampsFleetVersion(t *testing.T) {
	for _, tt := range []struct{ version, wantLabel string }{
		{"6.22.1", "6.22.1"},
		{"dev", "dev"},
		{"feat/some-branch", ""},
	} {
		c := newTestClient()
		c.Version = tt.version
		ctx := context.Background()
		if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
			SessionID: "task-1", Repo: "r", RepoURL: "https://github.com/o/r.git", BaseBranch: "main", LeaseID: "l",
			RepoVault: &RepoVault{Vault: "owner-o", ProxyURL: "http://tok@vault:1", DeadlineSeconds: 60, VaultLabel: "task-1.1"},
		}); err != nil {
			t.Fatalf("%s: %v", tt.version, err)
		}
		job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for what, labels := range map[string]map[string]string{"job": job.Labels, "pod": job.Spec.Template.Labels} {
			if got := labels[VersionLabel]; got != tt.wantLabel {
				t.Errorf("%s: %s label = %q, want %q", tt.version, what, got, tt.wantLabel)
			}
		}
		spec := job.Spec.Template.Spec
		for _, ctr := range append(spec.Containers, spec.InitContainers...) {
			for _, e := range ctr.Env {
				if e.Name == "FLEET_VERSION" {
					t.Errorf("%s: %s container carries FLEET_VERSION", tt.version, ctr.Name)
				}
			}
		}
	}
}
