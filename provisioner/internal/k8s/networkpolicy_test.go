package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// TestNetworkPolicies_AdmitExactlyTheKnownCallers pins the ingress fences of
// docs/adr/0069 §3 against the labels the callers actually carry. The labels
// live in three places that never change together: WorkerLabels here,
// k8s/provisioner/deployment.yaml, and the policies themselves (one in
// k8s/provisioner/, one in k8s/core.yaml's extraManifests). A label rename in
// any of them is a silent outage: every sidecar loses CoreService, or core
// loses the provisioner, while every manifest still applies cleanly.
//
// This evaluates plain NetworkPolicy semantics, not Cilium. That Cilium
// enforces them the same way (ClusterIP and pod IP, across namespaces) was
// proved live in a scratch namespace before they shipped.
//
// Reads files outside the module, like TestCreateWorkerPod_ResourcesWithinLimitRange:
// run with -count=1.
func TestNetworkPolicies_AdmitExactlyTheKnownCallers(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	read := func(rel string) []byte {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s — this guard is worthless if it silently skips: %v", rel, err)
		}
		return raw
	}

	var coreValues struct {
		ExtraManifests []string `json:"extraManifests"`
	}
	if err := yaml.Unmarshal(read("k8s/core.yaml"), &coreValues); err != nil {
		t.Fatalf("parse k8s/core.yaml: %v", err)
	}
	// extraManifests is tpl-rendered by common-app-chart; this is the one
	// release it is deployed as (infra-bootstrap gitops/apps/registry.yaml).
	release := strings.NewReplacer("{{ .Release.Name }}", "agent-fleet-core", "{{ .Release.Namespace }}", "agent-fleet")
	var corePolicy *networkingv1.NetworkPolicy
	for _, m := range coreValues.ExtraManifests {
		var np networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(release.Replace(m)), &np); err != nil {
			t.Fatalf("parse k8s/core.yaml extraManifests entry: %v", err)
		}
		if np.Kind == "NetworkPolicy" {
			corePolicy = &np
		}
	}
	if corePolicy == nil {
		t.Fatal("k8s/core.yaml extraManifests carries no NetworkPolicy")
	}

	var provPolicy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal(read("k8s/provisioner/networkpolicy.yaml"), &provPolicy); err != nil {
		t.Fatalf("parse k8s/provisioner/networkpolicy.yaml: %v", err)
	}
	var provDeploy appsv1.Deployment
	if err := yaml.Unmarshal(read("k8s/provisioner/deployment.yaml"), &provDeploy); err != nil {
		t.Fatalf("parse k8s/provisioner/deployment.yaml: %v", err)
	}

	type pod struct {
		ns     string
		labels map[string]string
	}
	// common-app-chart's selectorLabels: both keys are the release name.
	core := pod{"agent-fleet", map[string]string{
		"app.kubernetes.io/name": "agent-fleet-core", "app.kubernetes.io/instance": "agent-fleet-core",
	}}
	provisioner := pod{"agent-fleet", provDeploy.Spec.Template.Labels}
	worker := pod{"agent-fleet", WorkerLabels("t", "r")}
	sharedInstance := pod{"agent-fleet", SharedInstanceLabels("r", "postgres")}
	traefik := pod{"traefik", map[string]string{"app.kubernetes.io/name": "traefik"}}
	prometheus := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "prometheus"}}
	alertmanager := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "alertmanager"}}
	grafana := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "grafana"}}
	lokiInMonitoring := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "loki"}}
	// The same labels from another namespace must not pass: the selectors are
	// namespace-scoped, not label-only.
	foreignWorker := pod{"dev-x", WorkerLabels("t", "r")}
	foreignCore := pod{"dev-x", core.labels}

	admits := func(np *networkingv1.NetworkPolicy, target, from pod, port int) bool {
		if target.ns != np.Namespace || !selects(t, &np.Spec.PodSelector, target.labels) {
			t.Fatalf("policy %s does not select its target pod %v — it would fence nothing", np.Name, target.labels)
		}
		for _, rule := range np.Spec.Ingress {
			portOK := len(rule.Ports) == 0
			for _, p := range rule.Ports {
				portOK = portOK || p.Port.IntValue() == port
			}
			if !portOK {
				continue
			}
			if len(rule.From) == 0 {
				return true
			}
			for _, peer := range rule.From {
				nsOK := peer.NamespaceSelector == nil && from.ns == np.Namespace ||
					peer.NamespaceSelector != nil && selects(t, peer.NamespaceSelector, map[string]string{"kubernetes.io/metadata.name": from.ns})
				if nsOK && (peer.PodSelector == nil || selects(t, peer.PodSelector, from.labels)) {
					return true
				}
			}
		}
		return false
	}

	for _, tc := range []struct {
		name         string
		np           *networkingv1.NetworkPolicy
		target, from pod
		port         int
		want         bool
	}{
		{"sidecar -> CoreService", corePolicy, core, worker, 9090, true},
		{"provisioner -> CoreService", corePolicy, core, provisioner, 9090, true},
		{"traefik -> console", corePolicy, core, traefik, 8080, true},
		{"alertmanager -> /webhook/alertmanager", corePolicy, core, alertmanager, 8080, true},
		{"grafana -> /webhook/alertmanager", corePolicy, core, grafana, 8080, true},
		{"prometheus -> core metrics", corePolicy, core, prometheus, 9093, true},
		{"worker -> console port (agent-fleet#200)", corePolicy, core, worker, 8080, false},
		{"worker -> core metrics", corePolicy, core, worker, 9093, false},
		{"shared instance -> CoreService", corePolicy, core, sharedInstance, 9090, false},
		{"traefik -> CoreService", corePolicy, core, traefik, 9090, false},
		{"prometheus -> CoreService", corePolicy, core, prometheus, 9090, false},
		{"other monitoring pod -> console port", corePolicy, core, lokiInMonitoring, 8080, false},
		{"foreign namespace, worker labels -> CoreService", corePolicy, core, foreignWorker, 9090, false},

		{"core -> ProvisionerService", &provPolicy, provisioner, core, 9090, true},
		{"prometheus -> provisioner metrics", &provPolicy, provisioner, prometheus, 8080, true},
		{"worker -> ProvisionerService", &provPolicy, provisioner, worker, 9090, false},
		{"worker -> provisioner metrics", &provPolicy, provisioner, worker, 8080, false},
		{"prometheus -> ProvisionerService", &provPolicy, provisioner, prometheus, 9090, false},
		{"foreign namespace, core labels -> ProvisionerService", &provPolicy, provisioner, foreignCore, 9090, false},
	} {
		if got := admits(tc.np, tc.target, tc.from, tc.port); got != tc.want {
			t.Errorf("%s (:%d): admitted=%v, want %v", tc.name, tc.port, got, tc.want)
		}
	}
}

func selects(t *testing.T, sel *metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatalf("invalid label selector %v: %v", sel, err)
	}
	return s.Matches(labels.Set(l))
}
