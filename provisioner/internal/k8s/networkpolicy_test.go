package k8s

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// TestNetworkPolicies_AdmitExactlyTheKnownCallers checks the ingress fences of
// docs/adr/0069 §3. It pins the in-repo labels and ports, which live in places
// that never change together: WorkerLabels and SharedInstanceLabels here,
// k8s/provisioner/deployment.yaml (labels, containerPorts, PORT/GRPC_PORT),
// k8s/core.yaml's service ports and CORE_*_PORT env, and the policies
// themselves. A rename or renumber in any of them is a silent outage: every
// sidecar loses CoreService, core loses the provisioner, or Prometheus loses a
// target, while every manifest still applies cleanly. It also pins both
// policies to ingress only: an Egress policyType here would cut core and the
// provisioner off from everything they call (egress is ADR-0064's).
//
// The external callers (Traefik, Prometheus, Alertmanager, Grafana) are
// literals copied from live pods on 2026-10-18, and core's release name is
// copied from infra-bootstrap's gitops/apps/registry.yaml, which this repo
// cannot read. Their cases only assert the policy's shape, including the
// namespace-AND-pod peers. A relabel in infra-bootstrap's charts, or a rename
// of core's Application, cannot fail this test; §3 names what catches it.
//
// This evaluates plain NetworkPolicy semantics, not Cilium, and refuses the
// parts it does not model (ipBlock, endPort, named ports, non-TCP).
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
		Service        struct {
			Port       int `json:"port"`
			ExtraPorts []struct {
				Name       string `json:"name"`
				Port       int    `json:"port"`
				TargetPort int    `json:"targetPort"`
			} `json:"extraPorts"`
		} `json:"service"`
		Env []corev1.EnvVar `json:"env"`
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

	// The ports each pod really listens on, cross-checked against every place
	// that names them, so a renumber that misses the policy fails here.
	envOf := func(env []corev1.EnvVar, name string) string {
		for _, e := range env {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}
	corePorts := map[string]int{"http": coreValues.Service.Port}
	for _, p := range coreValues.Service.ExtraPorts {
		corePorts[p.Name] = p.Port
		if p.TargetPort != 0 {
			corePorts[p.Name] = p.TargetPort
		}
	}
	for name, env := range map[string]string{"http": "CORE_PORT", "grpc": "CORE_GRPC_PORT", "metrics": "CORE_METRICS_PORT"} {
		if got := envOf(coreValues.Env, env); got == "" || got != strconv.Itoa(corePorts[name]) {
			t.Fatalf("k8s/core.yaml: %s=%q but the %q service port targets %d", env, got, name, corePorts[name])
		}
	}
	provC := provDeploy.Spec.Template.Spec.Containers[0]
	provPorts := map[string]int{}
	for _, p := range provC.Ports {
		provPorts[p.Name] = int(p.ContainerPort)
	}
	for name, env := range map[string]string{"http": "PORT", "grpc": "GRPC_PORT"} {
		if got := envOf(provC.Env, env); got == "" || got != strconv.Itoa(provPorts[name]) {
			t.Fatalf("k8s/provisioner/deployment.yaml: %s=%q but containerPort %q is %d", env, got, name, provPorts[name])
		}
	}

	for _, np := range []*networkingv1.NetworkPolicy{corePolicy, &provPolicy} {
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(np.Spec.Egress) != 0 {
			t.Fatalf("policy %s: policyTypes %v, %d egress rules; these fences are ingress only (egress is ADR-0064's)", np.Name, np.Spec.PolicyTypes, len(np.Spec.Egress))
		}
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
				if p.EndPort != nil || p.Port == nil || p.Port.Type != intstr.Int || p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
					t.Fatalf("policy %s: port %v is a range, absent, named or not TCP; this evaluator only models int TCP ports", np.Name, p)
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
		{"sidecar -> CoreService", corePolicy, core, worker, corePorts["grpc"], true},
		{"provisioner -> CoreService", corePolicy, core, provisioner, corePorts["grpc"], true},
		{"traefik -> console", corePolicy, core, traefik, corePorts["http"], true},
		{"alertmanager -> /webhook/alertmanager", corePolicy, core, alertmanager, corePorts["http"], true},
		{"grafana -> /webhook/alertmanager", corePolicy, core, grafana, corePorts["http"], true},
		{"prometheus -> core metrics", corePolicy, core, prometheus, corePorts["metrics"], true},
		{"worker -> console port, direct (via Traefik it is admitted; the gate is the boundary)", corePolicy, core, worker, corePorts["http"], false},
		{"worker -> core metrics", corePolicy, core, worker, corePorts["metrics"], false},
		{"shared instance -> CoreService", corePolicy, core, sharedInstance, corePorts["grpc"], false},
		{"traefik -> CoreService", corePolicy, core, traefik, corePorts["grpc"], false},
		{"prometheus -> CoreService", corePolicy, core, prometheus, corePorts["grpc"], false},
		{"other monitoring pod -> console port", corePolicy, core, lokiInMonitoring, corePorts["http"], false},
		{"other monitoring pod -> core metrics", corePolicy, core, lokiInMonitoring, corePorts["metrics"], false},
		{"foreign namespace, prometheus labels -> core metrics", corePolicy, core, foreignPrometheus, corePorts["metrics"], false},
		{"foreign namespace, worker labels -> CoreService", corePolicy, core, foreignWorker, corePorts["grpc"], false},

		{"core -> ProvisionerService", &provPolicy, provisioner, core, provPorts["grpc"], true},
		{"prometheus -> provisioner metrics", &provPolicy, provisioner, prometheus, provPorts["http"], true},
		{"worker -> ProvisionerService", &provPolicy, provisioner, worker, provPorts["grpc"], false},
		{"worker -> provisioner metrics", &provPolicy, provisioner, worker, provPorts["http"], false},
		{"prometheus -> ProvisionerService", &provPolicy, provisioner, prometheus, provPorts["grpc"], false},
		{"other monitoring pod -> provisioner metrics", &provPolicy, provisioner, lokiInMonitoring, provPorts["http"], false},
		{"foreign namespace, prometheus labels -> provisioner metrics", &provPolicy, provisioner, foreignPrometheus, provPorts["http"], false},
		{"foreign namespace, core labels -> ProvisionerService", &provPolicy, provisioner, foreignCore, provPorts["grpc"], false},
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
