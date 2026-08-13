package k8s

import (
	"context"
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
