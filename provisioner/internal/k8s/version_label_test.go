package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// The release that built a pod rides on the Job and pod as a label and on
// both containers as FLEET_VERSION; a version that is not a valid label value
// keeps the env and drops only the label, rather than failing the create.
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
			if ctr.Name != "worker" && ctr.Name != "sidecar" {
				continue
			}
			if got, ok := envValue(ctr.Env, "FLEET_VERSION"); !ok || got != tt.version {
				t.Errorf("%s: %s FLEET_VERSION = %q (set %v)", tt.version, ctr.Name, got, ok)
			}
		}
	}
}
