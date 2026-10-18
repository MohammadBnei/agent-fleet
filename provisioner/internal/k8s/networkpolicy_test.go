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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// TestNetworkPolicies_AdmitExactlyTheKnownCallers checks the ingress fences of
// docs/adr/0069 §3. It pins the in-repo labels, which live in places that
// never change together: WorkerLabels and SharedInstanceLabels here,
// k8s/provisioner/deployment.yaml, core's release name, and the policies
// themselves. A rename in any of them is a silent outage: every sidecar loses
// CoreService, or core loses the provisioner, while every manifest still
// applies cleanly.
//
// The external callers (Traefik, Prometheus, Alertmanager, Grafana) are
// literals copied from live pods on 2026-10-18, so their cases only assert the
// policy's shape, including the namespace-AND-pod peers. A relabel in
// infra-bootstrap's charts cannot fail this test; §3 names the alerts that
// catch it.
//
// This evaluates plain NetworkPolicy semantics, not Cilium, and refuses the
// parts it does not model (ipBlock, endPort, named ports).
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
	// extraManifests is tpl-rendered by common-app-chart; coreRelease is the one
	// release it is deployed as (infra-bootstrap gitops/apps/registry.yaml).
	const coreRelease, ns = "agent-fleet-core", "agent-fleet"
	release := strings.NewReplacer("{{ .Release.Name }}", coreRelease, "{{ .Release.Namespace }}", ns)
	var corePolicy *networkingv1.NetworkPolicy
	for _, m := range coreValues.ExtraManifests {
		rendered := release.Replace(m)
		var tm metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(rendered), &tm); err != nil {
			t.Fatalf("parse k8s/core.yaml extraManifests entry: %v", err)
		}
		if tm.Kind != "NetworkPolicy" {
			continue
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("k8s/core.yaml NetworkPolicy uses a template this test does not render:\n%s", rendered)
		}
		if corePolicy != nil {
			t.Fatal("k8s/core.yaml extraManifests carries more than one NetworkPolicy; this test checks only one")
		}
		corePolicy = &networkingv1.NetworkPolicy{}
		if err := yaml.Unmarshal([]byte(rendered), corePolicy); err != nil {
			t.Fatalf("parse k8s/core.yaml NetworkPolicy: %v", err)
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
	core := pod{ns, map[string]string{
		"app.kubernetes.io/name": coreRelease, "app.kubernetes.io/instance": coreRelease,
	}}
	provisioner := pod{ns, provDeploy.Spec.Template.Labels}
	worker := pod{ns, WorkerLabels("t", "r")}
	sharedInstance := pod{ns, SharedInstanceLabels("r", "postgres")}
	traefik := pod{"traefik", map[string]string{"app.kubernetes.io/name": "traefik"}}
	prometheus := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "prometheus"}}
	alertmanager := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "alertmanager"}}
	grafana := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "grafana"}}
	lokiInMonitoring := pod{"monitoring", map[string]string{"app.kubernetes.io/name": "loki"}}
	// The same labels from another namespace must not pass: the selectors are
	// namespace-scoped, not label-only.
	foreignWorker := pod{"dev-x", WorkerLabels("t", "r")}
	foreignCore := pod{"dev-x", core.labels}
	foreignPrometheus := pod{"dev-x", prometheus.labels}

	admits := func(np *networkingv1.NetworkPolicy, target, from pod, port int) bool {
		if target.ns != np.Namespace || !selects(t, &np.Spec.PodSelector, target.labels) {
			t.Fatalf("policy %s does not select its target pod %v — it would fence nothing", np.Name, target.labels)
		}
		for _, rule := range np.Spec.Ingress {
			portOK := len(rule.Ports) == 0
			for _, p := range rule.Ports {
				if p.EndPort != nil || p.Port == nil || p.Port.Type != intstr.Int {
					t.Fatalf("policy %s: port %v is a range, absent or named; this evaluator only models int ports", np.Name, p)
				}
				portOK = portOK || p.Port.IntValue() == port
			}
			if !portOK {
				continue
			}
			if len(rule.From) == 0 {
				return true
			}
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					t.Fatalf("policy %s: ipBlock peer; this evaluator does not model it", np.Name)
				}
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
		{"other monitoring pod -> core metrics", corePolicy, core, lokiInMonitoring, 9093, false},
		{"foreign namespace, prometheus labels -> core metrics", corePolicy, core, foreignPrometheus, 9093, false},
		{"foreign namespace, worker labels -> CoreService", corePolicy, core, foreignWorker, 9090, false},

		{"core -> ProvisionerService", &provPolicy, provisioner, core, 9090, true},
		{"prometheus -> provisioner metrics", &provPolicy, provisioner, prometheus, 8080, true},
		{"worker -> ProvisionerService", &provPolicy, provisioner, worker, 9090, false},
		{"worker -> provisioner metrics", &provPolicy, provisioner, worker, 8080, false},
		{"prometheus -> ProvisionerService", &provPolicy, provisioner, prometheus, 9090, false},
		{"other monitoring pod -> provisioner metrics", &provPolicy, provisioner, lokiInMonitoring, 8080, false},
		{"foreign namespace, prometheus labels -> provisioner metrics", &provPolicy, provisioner, foreignPrometheus, 8080, false},
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
