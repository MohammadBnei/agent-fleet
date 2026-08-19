package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /auth/me must answer a SIGNED-OUT caller with 200 and authenticated:false,
// not 401.
//
// It is exempt from the gate on purpose — a page cannot ask "am I signed in"
// through a gate that refuses it for not being signed in. And the status code
// matters beyond politeness: the SPA's transport turns 401 into a redirect to
// /auth/login, so answering 401 here would bounce a signed-out visitor to
// authentik before the page rendered anything.
func TestMe_AnswersSignedOutWithoutA401(t *testing.T) {
	o := &OIDC{signer: mustSigner(t, "key-one")}

	rec := httptest.NewRecorder()
	o.Me(rec, httptest.NewRequest(http.MethodGet, "/auth/me", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("signed out: got %d, want 200 — a 401 here makes the SPA redirect on page load", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"authenticated":false`) {
		t.Errorf("signed out body = %s, want authenticated:false", body)
	}
	// Never a stale identity out of a cache.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestMe_ReturnsTheVerifiedClaim(t *testing.T) {
	s := mustSigner(t, "key-one")
	o := &OIDC{signer: s}

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: s.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}}),
	})
	rec := httptest.NewRecorder()
	o.Me(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"authenticated":true`) || !strings.Contains(body, "me@bnei.dev") {
		t.Errorf("body = %s, want the signed-in identity", body)
	}
}

// A forged or expired cookie must read as signed out rather than as an
// identity. This endpoint is exempt from the gate, so its own verification is
// the only check standing between a cookie and a rendered email.
func TestMe_TreatsAnInvalidCookieAsSignedOut(t *testing.T) {
	o := &OIDC{signer: mustSigner(t, "key-one")}
	forged := mustSigner(t, "attacker-key").Sign(Identity{Email: "attacker@evil.com"})

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: forged})
	rec := httptest.NewRecorder()
	o.Me(rec, req)

	if body := rec.Body.String(); strings.Contains(body, "attacker@evil.com") {
		t.Errorf("a cookie signed with an unknown key produced an identity: %s", body)
	}
}
