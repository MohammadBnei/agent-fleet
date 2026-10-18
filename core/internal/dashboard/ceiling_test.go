package dashboard

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

// Drift guard (docs/adr/0062): the definitions ceiling may never name a model
// or mode the console itself would refuse, or a valid definition would create
// sessions that fail at their first warm.
func TestDefinitionCeilingWithinConsoleAllowlists(t *testing.T) {
	for _, m := range definitions.ModelCeiling {
		if !validModels[m] {
			t.Errorf("definitions.ModelCeiling has %q, which validModels does not", m)
		}
	}
	for _, m := range definitions.ModeCeiling {
		if !validPermissionModes[m] {
			t.Errorf("definitions.ModeCeiling has %q, which validPermissionModes does not", m)
		}
	}
}
