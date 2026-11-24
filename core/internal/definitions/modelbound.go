package definitions

import (
	"fmt"
	"slices"
)

// ModelBound is the one rule for which models a definition may run on, used
// by create, restart, the console write and the console list: the model must
// be in ModelCeiling, and in the definition's effective model.allow when the
// definition sets a model. "" when allowed, else the reason.
func ModelBound(eff *Spec, model string) string {
	if !slices.Contains(ModelCeiling, model) {
		return fmt.Sprintf("%q is not in the fleet's model ceiling", model)
	}
	if eff != nil && eff.Model != nil && !slices.Contains(eff.Model.Allow, model) {
		return fmt.Sprintf("%q is not in the definition's model.allow %v", model, eff.Model.Allow)
	}
	return ""
}
