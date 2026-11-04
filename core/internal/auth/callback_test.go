package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// Callback, end to end against a fake IdP: discovery, JWKS and a token
// endpoint minting an RS256 ID token with the nonce Login put in the URL.
func TestCallback_MintsTheSessionFromTheIDToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	var claims map[string]any // set per case; nonce filled in from Login

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := srv.URL + "/application/o/fleet/"
	mux.HandleFunc("/application/o/fleet/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": srv.URL + "/authorize/",
			"token_endpoint": srv.URL + "/token/", "jwks_uri": srv.URL + "/jwks/",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token/", func(w http.ResponseWriter, _ *http.Request) {
		head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		body, _ := json.Marshal(claims)
		signing := b64(head) + "." + b64(body)
		sum := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a", "token_type": "Bearer", "id_token": signing + "." + b64(sig),
		})
	})

	o, err := New(context.Background(), Config{IssuerURL: issuer, ClientID: "client", ClientSecret: "s",
		PublicURL: "https://fleet.bnei.dev", SessionKeys: "key-one"})
	if err != nil {
		t.Fatal(err)
	}

	var authTime any // auth_time claim; nil leaves it out
	loginAt := func(path, sub string, groups []string) (*httptest.ResponseRecorder, Identity) {
		rec := httptest.NewRecorder()
		o.Login(rec, httptest.NewRequest(http.MethodGet, path, nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		state := rec.Result().Cookies()[0]
		claims = map[string]any{"iss": issuer, "aud": "client", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": loc.Query().Get("nonce"), "sub": sub,
			"email": "me@bnei.dev", "groups": groups}
		if authTime != nil {
			claims["auth_time"] = authTime
		}

		req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+loc.Query().Get("state"), nil)
		req.AddCookie(state)
		rec = httptest.NewRecorder()
		o.Callback(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == SessionCookieName && c.Value != "" {
				id, _ := o.signer.Verify(c.Value)
				return rec, id
			}
		}
		return rec, Identity{}
	}
	login := func(sub string, groups []string) (*httptest.ResponseRecorder, Identity) {
		return loginAt("/auth/login", sub, groups)
	}

	if rec, id := login("sub-123", []string{RequiredGroup}); rec.Code != http.StatusFound || id.Sub != "sub-123" || id.Email != "me@bnei.dev" {
		t.Errorf("admin login: %d, identity %+v; want 302 carrying sub", rec.Code, id)
	}
	if rec, id := login("", []string{RequiredGroup}); rec.Code != http.StatusUnauthorized || id.Email != "" {
		t.Errorf("no sub: %d, identity %+v; want 401 and no session", rec.Code, id)
	}
	if rec, id := login("sub-123", []string{"opus-users"}); rec.Code != http.StatusForbidden || id.Email != "" {
		t.Errorf("non-admin: %d, identity %+v; want 403 and no session", rec.Code, id)
	}
	// docs/adr/0082 §7: auth_time rides into the cookie; a reauth login
	// fails closed without a recent one.
	authTime = time.Now().Add(-time.Hour).Unix()
	if rec, id := login("sub-123", []string{RequiredGroup}); rec.Code != http.StatusFound || id.AuthAt != authTime {
		t.Errorf("auth_time: %d, identity %+v; want AuthAt %v", rec.Code, id, authTime)
	}
	if rec, id := loginAt("/auth/login?reauth=1", "sub-123", []string{RequiredGroup}); rec.Code != http.StatusForbidden || id.Email != "" {
		t.Errorf("reauth, old auth_time: %d, identity %+v; want 403 and no session", rec.Code, id)
	}
	authTime = nil
	if rec, id := loginAt("/auth/login?reauth=1", "sub-123", []string{RequiredGroup}); rec.Code != http.StatusForbidden || id.Email != "" {
		t.Errorf("reauth, no auth_time: %d, identity %+v; want 403 and no session", rec.Code, id)
	}
	authTime = time.Now().Unix()
	if rec, id := loginAt("/auth/login?reauth=1", "sub-123", []string{RequiredGroup}); rec.Code != http.StatusFound || !Fresh(id, time.Now()) {
		t.Errorf("reauth, fresh: %d, identity %+v; want 302 and a fresh session", rec.Code, id)
	}
	authTime = nil

	// docs/adr/0066 §2: an admitted audience group logs in. What it may reach
	// is the Gate's business, not the callback's.
	o.admit = []string{"shared-notes"}
	if rec, id := login("sub-456", []string{"shared-notes"}); rec.Code != http.StatusFound || id.Sub != "sub-456" {
		t.Errorf("member: %d, identity %+v; want 302 carrying sub", rec.Code, id)
	}
	if rec, id := login("sub-456", []string{"opus-users"}); rec.Code != http.StatusForbidden || id.Email != "" {
		t.Errorf("unknown group: %d, identity %+v; want 403 and no session", rec.Code, id)
	}
}

// docs/adr/0082 §7: reauth=1 asks the provider for a fresh credential.
func TestLogin_ReauthForcesAFreshSignIn(t *testing.T) {
	o := &OIDC{oauth: &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://idp/authorize"}}}
	for path, want := range map[string]bool{"/auth/login": false, "/auth/login?reauth=1": true} {
		rec := httptest.NewRecorder()
		o.Login(rec, httptest.NewRequest(http.MethodGet, path, nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		q := loc.Query()
		if got := q.Get("prompt") == "login" && q.Has("max_age") && q.Get("max_age") == "0"; got != want {
			t.Errorf("%s: prompt=%q max_age=%q, want forced=%v", path, q.Get("prompt"), q.Get("max_age"), want)
		}
	}
}
