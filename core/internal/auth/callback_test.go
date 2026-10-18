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

	login := func(sub string, groups []string) (*httptest.ResponseRecorder, Identity) {
		rec := httptest.NewRecorder()
		o.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		state := rec.Result().Cookies()[0]
		claims = map[string]any{"iss": issuer, "aud": "client", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": loc.Query().Get("nonce"), "sub": sub,
			"email": "me@bnei.dev", "groups": groups}

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

	if rec, id := login("sub-123", []string{RequiredGroup}); rec.Code != http.StatusFound || id.Sub != "sub-123" || id.Email != "me@bnei.dev" {
		t.Errorf("admin login: %d, identity %+v; want 302 carrying sub", rec.Code, id)
	}
	if rec, id := login("", []string{RequiredGroup}); rec.Code != http.StatusUnauthorized || id.Email != "" {
		t.Errorf("no sub: %d, identity %+v; want 401 and no session", rec.Code, id)
	}
	if rec, id := login("sub-123", []string{"opus-users"}); rec.Code != http.StatusForbidden || id.Email != "" {
		t.Errorf("non-admin: %d, identity %+v; want 403 and no session", rec.Code, id)
	}
}
