package k8s

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEndpointsFor_MatchesPublishedServicePorts is the drift guard docs/adr/0045
// asks for. EndpointsFor derives addresses from names rather than reading them
// back from the API, which is what lets the roster exist without a registry —
// but it also means these ports and the ones CreateService publishes are two
// lists that have to agree, with nothing but this test making them.
//
// The failure it prevents is remote from its cause: a roster naming a port no
// Service exposes still looks fine on the wire, and only fails when the
// sidecar dials it, in another process, mid-session.
func TestEndpointsFor_MatchesPublishedServicePorts(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateService(ctx, "task-1"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := c.Core.CoreV1().Services(c.Namespace).Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	published := map[int32]string{}
	for _, p := range svc.Spec.Ports {
		published[p.Port] = p.Name
	}

	endpoints := EndpointsFor(c.Namespace, "task-1")
	if len(endpoints) == 0 {
		t.Fatal("EndpointsFor returned nothing — the sidecar would have nowhere to dial")
	}
	for _, e := range endpoints {
		_, portStr, err := net.SplitHostPort(e.Address)
		if err != nil {
			t.Fatalf("endpoint %q address %q is not host:port: %v", e.Name, e.Address, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			t.Fatalf("endpoint %q port %q is not numeric: %v", e.Name, portStr, err)
		}
		name, ok := published[int32(port)]
		if !ok {
			t.Errorf("endpoint %q advertises port %d, which CreateService does not publish", e.Name, port)
			continue
		}
		if name != e.Name {
			t.Errorf("endpoint %q maps to port %d, published under the name %q — the two lists have drifted apart", e.Name, port, name)
		}
	}
}

// TestEndpointsFor_AddressesAreAbsolute pins the trailing dot. Without it the
// name is relative under ndots:5 and every resolution walks the pod's
// five-entry search list first. Cheap to lose in a refactor, invisible when
// lost — it still resolves, just slower, so nothing fails.
func TestEndpointsFor_AddressesAreAbsolute(t *testing.T) {
	for _, e := range EndpointsFor("agent-fleet", "task-1") {
		host, _, err := net.SplitHostPort(e.Address)
		if err != nil {
			t.Fatalf("endpoint %q address %q is not host:port: %v", e.Name, e.Address, err)
		}
		if !strings.HasSuffix(host, ".") {
			t.Errorf("endpoint %q host %q is relative — needs a trailing dot to skip the search list", e.Name, host)
		}
	}
}

// TestEndpointsJSON_ShapeMatchesTheSidecarParser pins the cross-module seam.
//
// The sidecar unmarshals this exact string into its own independently-declared
// struct, in another Go module, joined only by json tags. Nothing else makes
// those two agree, and a rename on either side degrades silently: the sidecar
// parses zero endpoints, falls back to the relay, and everything keeps
// working — right up until the relay is deleted.
//
// Asserted as literal keys rather than by round-tripping through the same
// struct, because a round-trip would pass even if both sides renamed together
// in a way that broke a running pod's already-set env var.
func TestEndpointsJSON_ShapeMatchesTheSidecarParser(t *testing.T) {
	raw := EndpointsJSON("agent-fleet", "task-1")
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("EndpointsJSON is not valid JSON: %v (%s)", err, raw)
	}
	if len(parsed) != 2 {
		t.Fatalf("got %d endpoints, want 2 (playwright + exec)", len(parsed))
	}
	for _, e := range parsed {
		for _, key := range []string{"name", "address", "protocol", "path"} {
			if _, ok := e[key]; !ok {
				t.Errorf("endpoint %v is missing %q — the sidecar's e2eclient.Endpoint reads exactly these", e, key)
			}
		}
	}
	byName := map[string]map[string]any{}
	for _, e := range parsed {
		byName[e["name"].(string)] = e
	}
	if _, ok := byName[EndpointExec]; !ok {
		t.Errorf("no %q endpoint — run_command has nowhere to go", EndpointExec)
	}
	if _, ok := byName[EndpointPlaywright]; !ok {
		t.Errorf("no %q endpoint — browser tools have nowhere to go", EndpointPlaywright)
	}
}

// The sidecar container must actually carry the roster. It is set at
// worker-pod creation, long before any sandbox exists, which is only sound
// because EndpointsFor derives addresses from names — and it is what lets the
// FIRST run_command of a session dial directly instead of needing the relay
// to bootstrap an address. Without it the roster only ever arrives after a
// provision, and the relay can never be deleted.
func TestCreateWorkerPod_SidecarCarriesTheEndpointRoster(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "lease-1", "/workspace/worktrees/task-1", "", 0, nil, nil, nil); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs(c.Namespace).Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var raw string
	var found bool
	for _, ct := range job.Spec.Template.Spec.InitContainers {
		if ct.Name != "sidecar" {
			continue
		}
		for _, e := range ct.Env {
			if e.Name == "FLEET_ENDPOINTS" {
				raw, found = e.Value, true
			}
		}
	}
	if !found {
		t.Fatal("sidecar container has no FLEET_ENDPOINTS — the first run_command of every session would have no address and fall back to the relay")
	}
	if raw != EndpointsJSON(c.Namespace, "task-1") {
		t.Errorf("FLEET_ENDPOINTS = %q, want the roster for this task in this namespace", raw)
	}
	if !strings.Contains(raw, ResourceName("task-1")) {
		t.Errorf("FLEET_ENDPOINTS %q does not name this task's sandbox", raw)
	}
}

// TestEndpointsFor_IsNamespaceScoped guards the scratch/kind case: a roster
// built for one namespace must never hand out an address in another. The
// sidecar dials whatever it is told, so a stale namespace here reaches a
// different cluster tenant's sandbox rather than failing.
func TestEndpointsFor_IsNamespaceScoped(t *testing.T) {
	for _, e := range EndpointsFor("agent-fleet-scratch", "task-1") {
		if !strings.Contains(e.Address, ".agent-fleet-scratch.svc.") {
			t.Errorf("endpoint %q address %q is not scoped to the namespace it was built for", e.Name, e.Address)
		}
	}
}
