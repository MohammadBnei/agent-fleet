package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// RequiredGroup is the authentik group an identity must carry. Membership lives
// in infra-bootstrap's authentik-blueprint-groups.yaml — one place saying who
// operates this cluster, deliberately NOT authentik's built-in admins group, so
// that administering the IdP and operating the fleet stay separate powers.
const RequiredGroup = "platform-admins"

const (
	stateCookieName = "__Host-fleet_oidc_state"
	stateTTL        = 10 * time.Minute
)

// Config is what core needs to federate. Every field is required; see
// New's failure mode.
type Config struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	// PublicURL is core's own externally reachable base, used to build the
	// redirect URI. It must match the provider's registered redirect_uris
	// exactly — authentik's matching_mode is `strict`.
	PublicURL   string
	SessionKeys string
}

type OIDC struct {
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	signer   *Signer
}

// New builds the relying party. It performs OIDC DISCOVERY against the issuer,
// which is not an implementation detail worth skipping.
//
// authentik's userinfo endpoint is NOT underneath its issuer: the issuer is
// per-application (https://authentik.bnei.dev/application/o/fleet/) while
// userinfo is at /application/o/userinfo/. Anything that builds the userinfo URL
// by appending a path to the issuer produces a doubled path that 404s with an
// HTML body — which is precisely what took ArgoCD's login down on 2026-08-19
// (infra-bootstrap #189). Discovery returns absolute endpoint URLs and cannot
// make that mistake. Do not replace it with hardcoded paths.
func New(ctx context.Context, cfg Config) (*OIDC, error) {
	signer, err := NewSigner(cfg.SessionKeys)
	if err != nil {
		return nil, err
	}
	// Passed through EXACTLY as configured — no normalisation, and in
	// particular no TrimSuffix. OIDC compares the issuer byte-for-byte against
	// the `issuer` field of the discovery document, and authentik's is
	// per-application WITH a trailing slash
	// (https://authentik.bnei.dev/application/o/fleet/). Stripping it is not
	// tidying, it is a mismatch:
	//
	//   issuer URL provided to client ("…/o/fleet") did not match the issuer
	//   URL returned by provider ("…/o/fleet/")
	//
	// which crash-loops core, because this path fails closed on purpose. If a
	// future issuer needs a different shape, change the CONFIG value; this line
	// must keep handing over whatever it was given.
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
	}
	return &OIDC{
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  strings.TrimSuffix(cfg.PublicURL, "/") + "/auth/callback",
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		signer: signer,
	}, nil
}

func (o *OIDC) Signer() *Signer { return o.signer }

// stateClaim is what the state cookie carries. `Return` is why it is a cookie
// rather than only the `state` parameter: the parameter round-trips through
// authentik and back through the user's browser, so it is attacker-influenced;
// the cookie does not leave this origin.
type stateClaim struct {
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
}

// Login starts the authorization-code flow.
func (o *OIDC) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomString()
	if err != nil {
		http.Error(w, "cannot start login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString()
	if err != nil {
		http.Error(w, "cannot start login", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	// Carry where the user was going. core's Discord notification links
	// straight to /sessions/<id> — that is the fleet's ONE "a human is needed"
	// entry point, clicked exactly when somebody must act. Without this, every
	// notification lands them on the fleet root and they have to find the
	// session again by hand.
	claim := stateClaim{Nonce: nonce, Verifier: verifier, Return: safeReturnTo(r.URL.Query().Get("return_to"))}
	blob, _ := json.Marshal(claim)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state + "." + base64.RawURLEncoding.EncodeToString(blob),
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})

	http.Redirect(w, r, o.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
}

// Callback completes the flow.
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "login expired, try again", http.StatusBadRequest)
		return
	}
	wantState, blob, found := strings.Cut(cookie.Value, ".")
	if !found || wantState == "" || r.URL.Query().Get("state") != wantState {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		http.Error(w, "state malformed", http.StatusBadRequest)
		return
	}
	var claim stateClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		http.Error(w, "state malformed", http.StatusBadRequest)
		return
	}

	token, err := o.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(claim.Verifier))
	if err != nil {
		slog.Warn("auth: code exchange failed", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	idToken, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		slog.Warn("auth: id token verification failed", "error", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	// Without this the ID token is not bound to THIS authorization request.
	if idToken.Nonce != claim.Nonce {
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}

	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	id := Identity{Email: claims.Email, Groups: claims.Groups}
	if !id.InGroup(RequiredGroup) {
		// Logged with the groups actually received, because the likeliest
		// cause is not an unauthorised person: it is the provider not emitting
		// a `groups` claim at all, which fails closed for EVERYONE and looks
		// exactly like a core bug. See the property_mappings comment in
		// infra-bootstrap's authentik-blueprint-fleet.yaml.
		slog.Warn("auth: rejected login, required group missing",
			"email", claims.Email, "groups", claims.Groups, "required", RequiredGroup)
		http.Error(w, "not authorized for this fleet", http.StatusForbidden)
		return
	}

	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true})
	setSessionCookie(w, o.signer.Sign(id))
	slog.Info("auth: login", "email", id.Email)
	http.Redirect(w, r, claim.Return, http.StatusFound)
}

func (o *OIDC) Logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusFound)
}

// safeReturnTo refuses anything that is not a local path.
//
// A bare "//evil.com" is a protocol-relative URL that browsers follow
// off-origin, so checking only for a leading "/" is not enough — that is the
// classic open-redirect, and here it would be one reached through a login flow,
// which is where they are most convincing.
func safeReturnTo(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if u, err := url.Parse(raw); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return raw
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
