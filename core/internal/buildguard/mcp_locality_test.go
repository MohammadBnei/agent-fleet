package buildguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpClientImport is what a component speaking MCP *outbound* imports. The
// server half (mcp-go/server, mcp-go/mcp) is a different thing entirely and
// is deliberately not matched: the sidecar hosts an MCP server for the agent,
// and e2e-runner's execmcp hosts one for everyone else.
const mcpClientImport = `"github.com/mark3labs/mcp-go/client"`

// mayDialMCP are the two components that reach a sandbox for their own
// reasons, per docs/adr/0045:
//
//   - sidecar: the agent's run_command and browser tools
//   - core:    the dashboard's RestartE2EApp / GetE2EAppLog, human-initiated
//
// Neither is a relay. That distinction is the whole point of the ADR, and it
// is why this list is two entries rather than "nobody" or "anybody".
var mayDialMCP = map[string]bool{"sidecar": true, "core": true}

// TestOnlyDesignatedComponentsDialMCP is the structural half of docs/adr/0045.
//
// The ADR deleted a tool-call relay: agent -> sidecar -> core -> provisioner
// -> sandbox, in which core and the provisioner each carried traffic that was
// none of their business. Deleting the code is not the same as preventing its
// return — a future change that adds an MCP client back to the provisioner
// reintroduces the exact shape, and would look perfectly reasonable in review
// because the call site would be small and local.
//
// So the rule is expressed where it cannot be quietly re-broken: the
// provisioner must not import an MCP client at all. It no longer speaks MCP,
// and the day it does again, this fails.
//
// Reads files outside this module, so like every other test in this package it
// must run with -count=1 — a cached PASS would survive the very change it
// exists to catch.
func TestOnlyDesignatedComponentsDialMCP(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}

	var offenders []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || mayDialMCP[e.Name()] {
			continue
		}
		component := e.Name()
		err := filepath.WalkDir(filepath.Join(root, component), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil //nolint:nilerr // an unreadable tree is not this guard's business
			}
			// Generated protobuf/gRPC code never imports an MCP client, and
			// walking it is pure noise.
			if strings.Contains(path, "/gen/") {
				return nil
			}
			// local/kind/workspace-data is the kind harness's hostPath mount:
			// after a /kind-local run it holds real git clones of the target
			// repos, and when the target is agent-fleet itself that means a
			// full copy of this source tree per session. Walking it reports
			// this repo's own legitimate MCP clients back as violations —
			// found live, six sessions in. It is gitignored; it is not source.
			if strings.Contains(path, "local/kind/workspace-data/") {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			if strings.Contains(string(body), mcpClientImport) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", component, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these files dial MCP outbound from a component that must not (docs/adr/0045): %v\n"+
			"Only %v may hold an MCP client, and only because each dials a sandbox for its own work. "+
			"If a component needs a sandbox tool call, give it an endpoint from the roster — do not add a relay.",
			offenders, keysOf(mayDialMCP))
	}
}

// TestProvisionerDoesNotDependOnMCP is the same rule one level down, where it
// is even harder to undo by accident: the provisioner's go.mod must not
// require mcp-go at all.
//
// The import check above could be satisfied by a transitive dependency
// creeping back in; this one fails on the module graph, which is what actually
// gets vendored into the image.
func TestProvisionerDoesNotDependOnMCP(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "provisioner", "go.mod"))
	if err != nil {
		t.Fatalf("read provisioner/go.mod: %v", err)
	}
	if strings.Contains(string(body), "mark3labs/mcp-go") {
		t.Error("provisioner/go.mod requires mcp-go again — the provisioner stopped speaking MCP in docs/adr/0045, " +
			"and a dependency on it is the first step back to the deleted relay")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
