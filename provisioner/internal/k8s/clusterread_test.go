package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readJobLabels(t *testing.T, read bool) (corev1.PodSpec, map[string]string) {
	t.Helper()
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
		SessionID: "t1", Repo: "r", LeaseID: "l",
		Definition: &DefinitionProfile{Name: "d", ProxyURL: "http://p", DeadlineSeconds: 60, ClusterRead: read},
	}); err != nil {
		t.Fatal(err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("t1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return job.Spec.Template.Spec, job.Spec.Template.Labels
}

func hasMount(c corev1.Container, name string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ADR-0084 §6: cluster.read names the read ServiceAccount and puts its token in the sidecar only; no grant, no SA.
func TestClusterReadServiceAccount(t *testing.T) {
	ps, labels := readJobLabels(t, true)
	if labels[ClusterReadLabel] != "true" {
		t.Errorf("granted pod lacks %s", ClusterReadLabel)
	}
	if ps.ServiceAccountName != ReadServiceAccount {
		t.Errorf("serviceAccountName = %q, want %q", ps.ServiceAccountName, ReadServiceAccount)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("automount must stay off")
	}
	var sidecar bool
	for _, ic := range ps.InitContainers {
		if ic.Name == "sidecar" {
			sidecar = hasMount(ic, "kube-read")
		} else if hasMount(ic, "kube-read") {
			t.Errorf("%s mounts the read token", ic.Name)
		}
	}
	if !sidecar {
		t.Error("sidecar lacks the read token")
	}
	for _, ic := range ps.InitContainers {
		for _, m := range ic.VolumeMounts {
			if ic.Name == "sidecar" && m.Name == "kube-read" &&
				(m.MountPath != "/var/run/secrets/kubernetes.io/serviceaccount" || !m.ReadOnly) {
				t.Errorf("sidecar kube-read mount = %q readOnly %v", m.MountPath, m.ReadOnly)
			}
		}
	}
	for _, v := range ps.Volumes {
		if v.Name != "kube-read" {
			continue
		}
		src := v.Projected.Sources
		if len(src) != 2 || src[0].ServiceAccountToken == nil || src[1].ConfigMap == nil ||
			src[1].ConfigMap.Name != "kube-root-ca.crt" || src[1].ConfigMap.Items[0].Key != "ca.crt" {
			t.Errorf("kube-read projection = %+v", src)
		}
	}
	for _, c := range ps.Containers {
		if hasMount(c, "kube-read") {
			t.Errorf("%s mounts the read token", c.Name)
		}
	}

	ps, labels = readJobLabels(t, false)
	if _, ok := labels[ClusterReadLabel]; ok {
		t.Error("ungranted pod carries the label")
	}
	if ps.ServiceAccountName != "" {
		t.Errorf("ungranted pod has serviceAccountName %q", ps.ServiceAccountName)
	}
	for _, v := range ps.Volumes {
		if v.Name == "kube-read" {
			t.Error("ungranted pod has the read token volume")
		}
	}
}

// The sidecar serves kubectl_read only from these three variables; an ungranted pod must carry none of them.
func TestClusterReadSidecarEnv(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
		SessionID: "t2", Repo: "r", LeaseID: "l",
		Definition: &DefinitionProfile{Name: "d", ProxyURL: "http://p", DeadlineSeconds: 60, ClusterRead: true,
			ClusterLogNamespaces: []string{"agent-fleet", "monitoring"}, TrustSet: "AB"},
	}); err != nil {
		t.Fatal(err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("t2"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ic := range job.Spec.Template.Spec.InitContainers {
		if ic.Name == "sidecar" {
			for _, e := range ic.Env {
				got[e.Name] = e.Value
			}
		}
	}
	if got["FLEET_CLUSTER_READ"] != "1" || got["FLEET_CLUSTER_LOG_NAMESPACES"] != "agent-fleet,monitoring" || got["FLEET_TRUST_SET"] != "AB" {
		t.Errorf("sidecar env = %v", got)
	}
	ps, _ := readJobLabels(t, false)
	for _, ic := range ps.InitContainers {
		for _, e := range ic.Env {
			if e.Name == "FLEET_CLUSTER_READ" {
				t.Error("ungranted sidecar has FLEET_CLUSTER_READ")
			}
		}
	}
}
