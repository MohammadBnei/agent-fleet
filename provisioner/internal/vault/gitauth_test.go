package vault

import (
	"strings"
	"testing"
)

// Both GitHub git auth shapes must pass the vault's own field rules
// (broker.Auth.Validate, v0.40.0): bearer needs exactly token; basic needs
// username, optional password; every value a credential KEY name.
func TestGithubAuthShapes(t *testing.T) {
	for _, a := range []auth{gitAuth("GH_DEV_NOTES", false), gitAuth("GH_DEV_NOTES", true)} {
		key := func(s string) bool { return s != "" && strings.ToUpper(s) == s && !strings.ContainsAny(s, " -./") }
		switch a.Type {
		case "bearer":
			if !key(a.Token) || a.Username != "" || a.Password != "" {
				t.Errorf("bearer %+v", a)
			}
		case "basic":
			if !key(a.Username) || !key(a.Password) || a.Token != "" {
				t.Errorf("basic %+v", a)
			}
		default:
			t.Errorf("type %q", a.Type)
		}
	}
}
