package e2edial

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	roster := []Endpoint{
		{Name: "playwright", Address: "pod.:8931", Path: "/mcp"},
		{Name: EndpointExec, Address: "pod.:8932", Path: "/mcp"},
	}

	ep, ok := Find(roster, EndpointExec)
	if !ok {
		t.Fatal("exec endpoint not found in a roster that contains it")
	}
	if ep.Address != "pod.:8932" {
		t.Errorf("address = %q, want the exec entry's — picking by position rather than name would pass playwright's", ep.Address)
	}

	if _, ok := Find(nil, EndpointExec); ok {
		t.Error("an empty roster must report not-found, so the caller falls back to the relay")
	}
	if _, ok := Find(roster, "nonexistent"); ok {
		t.Error("an absent name must report not-found")
	}
}

// EndpointExec must equal the name the provisioner writes into the roster
// (k8s.EndpointExec). They are separate constants in separate modules; if
// they drift, Find silently misses and core quietly relays forever — working,
// but never taking the new path, and breaking outright once the relay is
// deleted.
func TestEndpointExecMatchesTheProvisionerName(t *testing.T) {
	if EndpointExec != "exec" {
		t.Errorf("EndpointExec = %q, want \"exec\" — this is the wire name provisioner/internal/k8s.EndpointExec writes", EndpointExec)
	}
}

// An unreachable sandbox must return a real error rather than blocking on a
// human-facing dashboard request. The address is in .invalid., reserved by
// RFC 2606 to never resolve.
func TestRunCommand_UnreachableSandboxErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := RunCommand(ctx, Endpoint{Name: EndpointExec, Address: "nothing-here.invalid.:8932", Path: "/mcp"}, "echo hi")
	if err == nil {
		t.Fatal("expected an error dialing an unresolvable sandbox")
	}
	// The message has to name the address: these two handlers are the human's
	// only window into a sandbox, and "context deadline exceeded" alone does
	// not say which pod failed to answer.
	if !strings.Contains(err.Error(), "nothing-here.invalid.") {
		t.Errorf("error %q does not name the address it failed to reach", err)
	}
}
