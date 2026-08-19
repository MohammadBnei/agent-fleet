package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discoveryStub serves an OIDC discovery document whose issuer ends in a
// trailing slash, which is the shape authentik actually returns: its issuer is
// per-application, e.g. https://authentik.bnei.dev/application/o/fleet/.
func discoveryStub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := srv.URL + "/application/o/fleet/"
	mux.HandleFunc("/application/o/fleet/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": srv.URL + "/application/o/authorize/",
			"token_endpoint":         srv.URL + "/application/o/token/",
			// Deliberately NOT under the issuer — authentik's userinfo lives at
			// /application/o/userinfo/, which is why discovery is used instead
			// of building endpoints by concatenation (infra-bootstrap #189).
			"userinfo_endpoint":                     srv.URL + "/application/o/userinfo/",
			"jwks_uri":                              srv.URL + "/application/o/fleet/jwks/",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	return srv
}

// New must hand the configured issuer to the OIDC library UNCHANGED.
//
// This shipped broken: the issuer was passed through strings.TrimSuffix(_, "/")
// as defensive normalisation. OIDC compares the issuer byte-for-byte against
// the discovery document, so stripping the slash produced
//
//	issuer URL provided to client ("…/o/fleet") did not match the issuer URL
//	returned by provider ("…/o/fleet/")
//
// and core crash-looped — this path fails closed on purpose, so a normalisation
// bug here is an outage rather than a degraded login. No unit test could see it
// before, because nothing exercised real discovery.
func TestNew_PassesTheIssuerThroughUnchanged(t *testing.T) {
	srv := discoveryStub(t)
	issuer := srv.URL + "/application/o/fleet/"

	o, err := New(context.Background(), Config{
		IssuerURL:    issuer,
		ClientID:     "client",
		ClientSecret: "secret",
		PublicURL:    "https://fleet.bnei.dev",
		SessionKeys:  "key-one",
	})
	if err != nil {
		t.Fatalf("discovery against an issuer with a trailing slash failed: %v", err)
	}

	// The redirect URI must still be normalised — that one is string
	// concatenation, not an identity compared by a remote party.
	if got := o.oauth.RedirectURL; got != "https://fleet.bnei.dev/auth/callback" {
		t.Errorf("RedirectURL = %q, want no doubled slash", got)
	}
}

// The failure has to stay loud. Silently continuing without a gate is the one
// outcome worse than crash-looping, since the host is publicly reachable.
func TestNew_FailsClosedOnBadConfig(t *testing.T) {
	srv := discoveryStub(t)

	for name, cfg := range map[string]Config{
		"no session keys":    {IssuerURL: srv.URL + "/application/o/fleet/", ClientID: "c", SessionKeys: ""},
		"unreachable issuer": {IssuerURL: srv.URL + "/application/o/does-not-exist/", ClientID: "c", SessionKeys: "k"},
	} {
		if _, err := New(context.Background(), cfg); err == nil {
			t.Errorf("%s: New returned a usable gate; it must fail closed", name)
		}
	}
}

// Guards the mistake directly, in case someone reintroduces "tidying" without
// running the discovery test above against a real provider shape.
func TestNew_DoesNotNormaliseTheIssuer(t *testing.T) {
	srv := discoveryStub(t)
	withSlash := srv.URL + "/application/o/fleet/"

	// The same issuer WITHOUT its trailing slash must be rejected, because that
	// is genuinely a different issuer as far as OIDC is concerned. If this ever
	// passes, something is normalising again.
	if _, err := New(context.Background(), Config{
		IssuerURL:   strings.TrimSuffix(withSlash, "/"),
		ClientID:    "c",
		SessionKeys: "k",
	}); err == nil {
		t.Error("an issuer missing its trailing slash was accepted — the value is compared byte-for-byte and must not be normalised on either side")
	}
}
