package vault

import "testing"

// The github.com git auth must pass the vault's own field rules
// (broker.Auth.Validate, v0.40.0): basic needs a username, optional password,
// every value a credential KEY name.
func TestGithubAuthShape(t *testing.T) {
	if a := gitAuth("GH_DEV_NOTES"); a != (auth{Type: "basic", Username: gitUsernameKey, Password: "GH_DEV_NOTES"}) {
		t.Errorf("%+v", a)
	}
}
