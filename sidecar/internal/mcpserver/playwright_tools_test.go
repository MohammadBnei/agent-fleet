package mcpserver

import (
	"encoding/json"
	"testing"
)

// The embedded snapshot is the only thing that makes browser tools visible to
// the agent now (docs/adr/0044), so a malformed or truncated file is a silent
// loss of a whole capability — registerPlaywrightTools logs and returns rather
// than crashing the sidecar. These tests are what turn that into a build-time
// failure instead.
func TestPlaywrightSnapshot_ParsesAndIsPopulated(t *testing.T) {
	var tools []playwrightTool
	if err := json.Unmarshal(playwrightToolsJSON, &tools); err != nil {
		t.Fatalf("embedded Playwright snapshot does not parse: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("embedded Playwright snapshot is empty — the agent would silently have no browser")
	}

	for _, tool := range tools {
		if tool.Name == "" {
			t.Error("a tool in the snapshot has no name")
		}
		if tool.Description == "" {
			t.Errorf("%s has no description — the agent picks tools by description", tool.Name)
		}
		// The schema has to survive the round-trip into mcp.ToolInputSchema,
		// which is what the agent is shown. A tool whose schema silently
		// decodes to nothing is worse than an absent one: it looks callable
		// and then rejects every argument.
		if len(tool.InputSchema) == 0 {
			t.Errorf("%s has no inputSchema", tool.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s has an unparseable inputSchema: %v", tool.Name, err)
		}
	}
}

// docs/adr/0039's load-bearing invariant, now that this list is a file a human
// regenerates rather than something filtered at runtime: run_command must
// never appear here. Registering it would overwrite New()'s static
// pod-provisioning handler with a plain passthrough that cannot provision.
func TestPlaywrightSnapshot_NeverContainsRunCommand(t *testing.T) {
	var tools []playwrightTool
	if err := json.Unmarshal(playwrightToolsJSON, &tools); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tool := range tools {
		if tool.Name == runCommandToolName {
			t.Fatalf("%q must never be in the Playwright snapshot — it would shadow the provisioning handler", runCommandToolName)
		}
	}
}

// A cheap smoke test that the snapshot is actually @playwright/mcp's and not,
// say, an empty array or some other server's list — catches a regeneration
// that pointed at the wrong port.
func TestPlaywrightSnapshot_HasTheCoreBrowserTools(t *testing.T) {
	var tools []playwrightTool
	if err := json.Unmarshal(playwrightToolsJSON, &tools); err != nil {
		t.Fatalf("parse: %v", err)
	}
	have := map[string]bool{}
	for _, tool := range tools {
		have[tool.Name] = true
	}
	for _, want := range []string{"browser_navigate", "browser_click", "browser_snapshot", "browser_take_screenshot"} {
		if !have[want] {
			t.Errorf("snapshot is missing %q — regenerated against the wrong server?", want)
		}
	}
}
