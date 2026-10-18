package k8s

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// The provisioner fails closed at boot (docs/adr/0069). Only a readiness probe
// on /healthz, which answers after that check, keeps a rollout with a bad
// Secret from deleting the working pod.
func TestDeployment_HasReadinessProbe(t *testing.T) {
	b := repoFile(t, "k8s/provisioner/deployment.yaml")
	var d appsv1.Deployment
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "provisioner" {
			continue
		}
		if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/healthz" {
			t.Fatalf("provisioner container needs readinessProbe httpGet /healthz, got %+v", p)
		}
		return
	}
	t.Fatal("no provisioner container in k8s/provisioner/deployment.yaml")
}
