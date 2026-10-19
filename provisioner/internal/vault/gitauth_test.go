package vault

import "testing"

// Pins the wire result of docs/adr/0065 §2's check (2026-10-19): github.com's
// git endpoint answered 200 to Basic x-access-token:<token> and 401 to Bearer
// <same token>, and git ls-remote of a private repo went through a v0.40.0
// vault with this shape. Switching to bearer breaks git, not this literal.
func TestGitAuthIsBasicXAccessToken(t *testing.T) {
	if a := gitAuth("GH_DEV_NOTES"); a != (auth{Type: "basic", Username: gitUsernameKey, Password: "GH_DEV_NOTES"}) {
		t.Errorf("%+v", a)
	}
}
