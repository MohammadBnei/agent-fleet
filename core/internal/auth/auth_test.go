package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustSigner(t *testing.T, keys string) *Signer {
	t.Helper()
	s, err := NewSigner(keys)
	if err != nil {
		t.Fatalf("NewSigner(%q): %v", keys, err)
	}
	return s
}

func TestSigner_RoundTrip(t *testing.T) {
	s := mustSigner(t, "key-one")
	id, ok := s.Verify(s.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}}))
	if !ok {
		t.Fatal("a cookie this signer just signed did not verify")
	}
	if id.Email != "me@bnei.dev" || !id.InGroup(RequiredGroup) {
		t.Errorf("round trip lost the identity: %+v", id)
	}
}

// Sign with the first, verify against any. That ordering IS the rotation
// mechanism, so it needs a test that exercises a rotation rather than one that
// only proves a signature works.
func TestSigner_RotationKeepsOldCookiesValidUntilTheKeyIsDropped(t *testing.T) {
	old := mustSigner(t, "key-A")
	cookie := old.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}})

	// New key prepended, old one still trailing: a live session survives the
	// rotation instead of everyone being logged out mid-decision.
	during := mustSigner(t, "key-B,key-A")
	if _, ok := during.Verify(cookie); !ok {
		t.Error("rotation logged out a live session: a cookie signed with the retiring key must still verify")
	}
	// And new cookies are signed with the NEW key, or the old one could never
	// be retired.
	fresh := during.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}})
	if _, ok := mustSigner(t, "key-B").Verify(fresh); !ok {
		t.Error("signed with the wrong key: rotation would never complete")
	}

	// Once dropped, it stops working. This is the revocation story.
	if _, ok := mustSigner(t, "key-B").Verify(cookie); ok {
		t.Error("a cookie signed with a DROPPED key still verifies — rotating revokes nothing")
	}
}

func TestSigner_RejectsTamperingAndJunk(t *testing.T) {
	s := mustSigner(t, "key-one")
	good := s.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}})
	payload, sig, _ := strings.Cut(good, ".")

	// A DIFFERENT identity, deliberately. Signing the same one in the same
	// second produces a byte-identical payload, so pairing it with the real
	// signature is just the real cookie — a test that proves nothing and
	// initially failed for exactly that reason.
	forged := mustSigner(t, "attacker-key").Sign(Identity{Email: "attacker@evil.com", Groups: []string{RequiredGroup}})
	for name, value := range map[string]string{
		"empty":            "",
		"no separator":     payload + sig,
		"payload swapped":  strings.Split(forged, ".")[0] + "." + sig,
		"signature only":   "." + sig,
		"garbage":          "not-a-cookie",
		"foreign key":      forged,
		"truncated base64": payload[:len(payload)-3] + "." + sig,
	} {
		if _, ok := s.Verify(value); ok {
			t.Errorf("%s verified", name)
		}
	}
}

// An empty key list must be an error, not a signer with a zero-length key.
// "An unset secret is the empty string, not an error" is already in this
// repo's trap list, and here it would mint valid sessions for anyone.
func TestNewSigner_RefusesAnEmptyOrBlankKeyList(t *testing.T) {
	for _, in := range []string{"", ",", "  ", " , , "} {
		if _, err := NewSigner(in); err == nil {
			t.Errorf("NewSigner(%q) returned a usable signer; an unset key must fail loudly", in)
		}
	}
}

// The __Host- prefix is not cosmetic: the browser rejects the cookie outright
// unless Secure and Path=/ are set, and forbids Domain. That is what stops an
// agent-authored dev server on <id>-e2e.bnei.dev — same-site with the console —
// from setting a Domain=bnei.dev cookie that shadows the operator's session.
func TestSessionCookie_CarriesTheHostPrefixContract(t *testing.T) {
	if !strings.HasPrefix(SessionCookieName, "__Host-") {
		t.Fatalf("cookie name %q lost its __Host- prefix", SessionCookieName)
	}
	rec := httptest.NewRecorder()
	setSessionCookie(rec, "value")

	c := rec.Result().Cookies()[0]
	if !c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly {
		t.Errorf("__Host- cookie must be Secure, Path=/, no Domain, HttpOnly; got %+v", c)
	}
}

func TestIdentity_InGroup(t *testing.T) {
	id := Identity{Groups: []string{"other", RequiredGroup}}
	if !id.InGroup(RequiredGroup) {
		t.Error("membership not detected")
	}
	if (Identity{Groups: []string{"platform-admins-readonly"}}).InGroup(RequiredGroup) {
		t.Error("prefix match accepted as membership")
	}
	if (Identity{}).InGroup(RequiredGroup) {
		t.Error("a token with NO groups claim was treated as a member — this is the failure mode when the provider simply does not emit `groups`, and it must fail closed")
	}
}

// An open redirect reached through a login flow is the most convincing kind.
// "//evil.com" is the case a naive HasPrefix("/") check waves through.
func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"/sessions/abc":           "/sessions/abc",
		"/?view=audits":           "/?view=audits",
		"":                        "/",
		"//evil.com":              "/",
		"https://evil.com":        "/",
		"http://evil.com/x":       "/",
		"evil.com":                "/",
		"//evil.com/sessions/abc": "/",
	} {
		if got := safeReturnTo(in); got != want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

// The gate is an allowlist, and a route that nobody exempted must fail closed.
// Gating one handler instead would make every future route public by default.
func TestGate_ExemptsOnlyWhatIsListed(t *testing.T) {
	exemptCases := []string{"/healthz", "/webhook/alertmanager", "/auth/login", "/auth/callback"}
	for _, p := range exemptCases {
		if !exempt(p) {
			t.Errorf("%s must be reachable without a session", p)
		}
	}
	// /metrics is deliberately NOT exempt: it is scraped in-cluster and has no
	// IngressRoute, so exempting it here would be an app-level workaround for
	// something that should not be externally routed in the first place.
	gatedCases := []string{"/", "/metrics", "/sessions/abc", "/agentfleet.v1.DashboardService/ListSessions", "/authsomething", "/some/new/route"}
	for _, p := range gatedCases {
		if exempt(p) {
			t.Errorf("%s is exempt from the session gate but should not be", p)
		}
	}
}

func TestGate_RedirectsDocumentsAndRefusesXHR(t *testing.T) {
	o := &OIDC{signer: mustSigner(t, "key-one")}
	handler := o.Gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// A person navigating gets sent to the login, carrying where they were
	// going — core's Discord notification links straight to /sessions/<id>,
	// which is the one moment somebody must act.
	nav := httptest.NewRequest(http.MethodGet, "/sessions/abc", nil)
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, nav)
	if rec.Code != http.StatusFound {
		t.Errorf("navigation: got %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "return_to=%2Fsessions%2Fabc") && !strings.Contains(loc, "return_to=/sessions/abc") {
		t.Errorf("navigation lost its destination: Location = %q", loc)
	}

	// The SPA's own call must get a status it can react to. A 302 would be
	// followed transparently and hand authentik's HTML to a JSON parser —
	// the exact shape that took ArgoCD's login down (infra-bootstrap #189).
	xhr := httptest.NewRequest(http.MethodPost, "/agentfleet.v1.DashboardService/ListSessions", nil)
	xhr.Header.Set("Sec-Fetch-Mode", "cors")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, xhr)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("xhr: got %d, want 401", rec.Code)
	}
}

func TestGate_LetsAValidSessionThroughAndCarriesTheIdentity(t *testing.T) {
	s := mustSigner(t, "key-one")
	o := &OIDC{signer: s}

	var seen string
	handler := o.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := FromContext(r.Context()); ok {
			seen = id.Email
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/sessions/abc", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: s.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}})})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a valid session was refused: %d", rec.Code)
	}
	if seen != "me@bnei.dev" {
		t.Errorf("identity did not reach the handler: %q", seen)
	}
}

// #200's actual reproduction: a curl from a worker pod carrying only the CSRF
// header. It has no session cookie, so it must fail regardless of which network
// it arrived from — the property a Traefik middleware structurally cannot give.
func TestGate_RefusesTheWorkerPodBypassFrom200(t *testing.T) {
	o := &OIDC{signer: mustSigner(t, "key-one")}
	handler := o.Gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the #200 bypass reached the handler")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/agentfleet.v1.DashboardService/GetJournal", strings.NewReader(`{"repo":"","sinceId":0,"limit":100000}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fleet-Dashboard", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
}
