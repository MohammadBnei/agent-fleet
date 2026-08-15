package buildguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpClientImport is what a component speaking MCP *outbound* imports. The
// server half (mcp-go/server, mcp-go/mcp) is a different thing entirely and is
// deliberately not matched: the sidecar hosts an MCP server for the agent, and
// that is the whole point of it.
const mcpClientImport = `"github.com/mark3labs/mcp-go/client"`

// mayDialMCP is EMPTY, and that is the assertion.
//
// docs/adr/0045 allowed two entries — sidecar and core — because each dialled
// a task's sandbox for its own reasons. docs/adr/0048 §6 deleted the sandbox,
// so there is nothing left in this fleet to dial: the agent's tools are on its
// own pod's localhost, and every inter-component call is gRPC.
//
// Left as a map rather than collapsed into "nobody" because the day someone
// has a real reason to add an entry, the list is where the argument goes.
var mayDialMCP = map[string]bool{}

// TestOnlyDesignatedComponentsDialMCP keeps a deleted shape deleted.
//
// docs/adr/0045 removed a tool-call relay — agent -> sidecar -> core ->
// provisioner -> sandbox — in which core and the provisioner each carried
// traffic that was none of their business. Deleting code is not the same as
// preventing its return: a change that adds an MCP client back to any of these
// reintroduces the exact shape, and would look perfectly reasonable in review
// because the call site would be small and local.
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
			// Test files are skipped, and this one has to be: it holds the
			// import path as a string constant, so with an empty allowlist the
			// guard reported ITSELF as the offender. A test naming the import
			// is not a component dialling MCP.
			if strings.HasSuffix(path, "_test.go") {
				return nil
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
		t.Errorf("these files dial MCP outbound, and nothing in this fleet should (docs/adr/0048 §6): %v\n"+
			"Currently allowed: %v (empty). There is no sandbox and no second pod — the agent's tools are on\n"+
			"its own pod's localhost, and everything between components is gRPC. If you genuinely need an\n"+
			"outbound MCP client, add the component to mayDialMCP with the reason, rather than letting a\n"+
			"relay grow back.",
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
