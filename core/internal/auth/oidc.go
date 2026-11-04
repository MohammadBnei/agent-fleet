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
//
// Checked at the callback AND on every request through AdminOnly (docs/adr/0069).
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
	// AdmitGroups is who may log in at all (docs/adr/0066 §2): the
	// definitions ceiling's known groups. Login is not access: the Gate keeps
	// everything but its member routes AdminOnly. Empty admits RequiredGroup
	// only.
	AdmitGroups []string
	// AdminOrigins are our app surfaces a login may return to as an absolute
	// URL (FLEET_ADMIN_ORIGINS, docs/adr/0082 §7): exact origins, never
	// merged with embed.origins.
	AdminOrigins []string
}

type OIDC struct {
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	signer   *Signer
	admit    []string
	origins  []string
}

// admits reports whether a login carrying id's groups is let in.
func (o *OIDC) admits(id Identity) bool {
	if id.InGroup(RequiredGroup) {
		return true
	}
	for _, g := range o.admit {
		if id.InGroup(g) {
			return true
		}
	}
	return false
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
		signer:  signer,
		admit:   cfg.AdmitGroups,
		origins: cfg.AdminOrigins,
	}, nil
}

// stateClaim is what the state cookie carries. `Return` is why it is a cookie
// rather than only the `state` parameter: the parameter round-trips through
// authentik and back through the user's browser, so it is attacker-influenced;
// the cookie does not leave this origin.
type stateClaim struct {
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	// Reauth marks a forced fresh sign-in (docs/adr/0082 §7): the callback
	// then refuses an ID token whose auth_time is missing or old.
	Reauth bool `json:"a,omitempty"`
}

// reauthWindow is how close to now a forced sign-in's auth_time must be. A
// provider that ignored prompt=login hands back the old auth_time and fails.
const reauthWindow = 2 * time.Minute

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
	claim := stateClaim{Nonce: nonce, Verifier: verifier,
		Return: SafeReturnTo(r.URL.Query().Get("return_to"), o.origins),
		Reauth: r.URL.Query().Get("reauth") == "1"}
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

	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if claim.Reauth {
		// Both, because a provider may honour only one; the callback checks
		// auth_time either way, so neither is trusted on its own.
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	http.Redirect(w, r, o.oauth.AuthCodeURL(state, opts...), http.StatusFound)
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
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
		AuthTime int64    `json:"auth_time"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	// OIDC makes sub mandatory and go-oidc does not check it. A session with
	// no subject could never be told apart from an old cookie's empty Sub.
	if idToken.Subject == "" {
		slog.Warn("auth: rejected login, id token has no sub", "email", claims.Email)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	// Fails closed: no auth_time means we cannot tell the sign-in was fresh.
	if claim.Reauth {
		if age := time.Since(time.Unix(claims.AuthTime, 0)); claims.AuthTime == 0 || age > reauthWindow || age < -reauthWindow {
			slog.Warn("auth: fresh sign-in refused", "email", claims.Email, "auth_time", claims.AuthTime)
			http.Error(w, "fresh sign-in failed", http.StatusForbidden)
			return
		}
	}

	id := Identity{Sub: idToken.Subject, Email: claims.Email, Groups: claims.Groups, AuthAt: claims.AuthTime}
	if !o.admits(id) {
		// Logged with the groups actually received, because the likeliest
		// cause is not an unauthorised person: it is the provider not emitting
		// a `groups` claim at all, which fails closed for EVERYONE and looks
		// exactly like a core bug. See the property_mappings comment in
		// infra-bootstrap's authentik-blueprint-fleet.yaml.
		slog.Warn("auth: rejected login, no admitted group",
			"email", claims.Email, "groups", claims.Groups, "admit", append([]string{RequiredGroup}, o.admit...))
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

// SafeReturnTo refuses anything that is not a local path or a URL on one of
// origins, compared exactly after parsing (docs/adr/0082 §7).
//
// A bare "//evil.com" is a protocol-relative URL that browsers follow
// off-origin, so checking only for a leading "/" is not enough — that is the
// classic open-redirect, and here it would be one reached through a login flow,
// which is where they are most convincing. Backslashes and control characters
// are refused outright: browsers read a backslash as "/", url.Parse does not.
func SafeReturnTo(raw string, origins []string) string {
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && u.Host == "" && u.Scheme == "" {
		return raw
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" || u.Opaque != "" {
		return "/"
	}
	for _, o := range origins {
		if o == "https://"+u.Host {
			return raw
		}
	}
	return "/"
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
