package agentsvc

import "testing"

// docs/adr/0070 §2: the request hash uses a constant where interaction was,
// so a claim stored with the real value ("ask", the only one Open took)
// still matches. It pins the constant only: Open needs a database to drive.
func TestRequestInteractionMatchesOldClaims(t *testing.T) {
	old := requestSHA("d", "k", "m", "false", "false", "", "", "shared", "ask", "resident")
	now := requestSHA("d", "k", "m", "false", "false", "", "", "shared", requestInteraction, "resident")
	if old != now {
		t.Fatal("hash with the constant differs from an old claim")
	}
}
