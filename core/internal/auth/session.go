// Package auth makes core an OIDC relying party against authentik and gates
// the console behind it (infra-bootstrap ADR-0041).
//
// WHY IN THE PROCESS AND NOT AT THE INGRESS: a Traefik forwardAuth middleware
// — which is what infra-bootstrap ADR-0039 originally assigned this host — has
// no opinion about a caller that never traverses the ingress. A worker pod
// dialling agent-fleet-core.agent-fleet.svc.cluster.local:8080 on the pod
// network is exactly that caller, and that is #200. A check here applies
// regardless of network position.
//
// This is now the ONLY gate on fleet.bnei.dev. basic-admin-auth fronted it for
// a few hours and was removed once a real login was proven (ADR-0041's
// amendment), so there is no second layer and no local admin behind this one.
//
// Two things follow, and both are load-bearing:
//
//   - Failing closed is not defensive styling here, it is the whole design. If
//     this package cannot build a working gate it must refuse to serve, because
//     the alternative on a host with no other lock is an open console. cmd/core
//     enforces that; FLEET_AUTH_DISABLED=1 is the explicit local-stack opt-out.
//   - An authentik outage — or Pigsty, or Patroni, which sit under it — means no
//     console at all, on the surface where a blocked session's permission
//     decision gets answered. Recovery is FLEET_AUTH_DISABLED plus a redeploy,
//     or kubectl port-forward. That cost was accepted knowingly; it did not go
//     away.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// SessionCookieName carries the __Host- prefix deliberately, and it is load-
// bearing rather than decorative. The browser then REFUSES the cookie unless it
// is Secure and Path=/, and forbids a Domain attribute entirely.
//
// That matters here specifically: the fleet publishes agent-authored dev
// servers at <id>-e2e.bnei.dev (provisioner's PreviewHostFor), which is
// same-site with fleet.bnei.dev. SameSite is no defence between same-site
// hosts, and without the prefix a preview page could set a Domain=bnei.dev
// cookie of the same name that shadows the operator's console session.
//
// What actually stops a preview page making authenticated RPCs is the
// pre-existing X-Fleet-Dashboard header plus never allowing CORS — that is the
// load-bearing control, not SameSite. Keep both.
const SessionCookieName = "__Host-fleet_session"

// sessionTTL is how long a login lasts. Deliberately not tied to the ID token's
// own expiry: this cookie is core's own statement about a browser, and there is
// no refresh flow here to renew it with.
const sessionTTL = 12 * time.Hour

// Identity is what a verified session asserts. Kept minimal on purpose — the
// fleet has no user model, and this is not one. It is the claim, carried far
// enough to authorize a request and attribute an action.
type Identity struct {
	Email  string   `json:"e"`
	Groups []string `json:"g"`
	Expiry int64    `json:"x"`
}

func (i Identity) InGroup(name string) bool {
	for _, g := range i.Groups {
		if g == name {
			return true
		}
	}
	return false
}

// Signer signs and verifies session cookies.
//
// Keys are ordered: sign with the FIRST, verify against ANY. That ordering is
// the entire key-rotation mechanism — prepend a new key, let the old one ride
// out live sessions, then drop it. No key store, no session table, no
// revocation list; the cost is that a captured cookie is good until it expires,
// and rotating the whole list is the blunt instrument that ends every session.
type Signer struct {
	keys [][]byte
}

var ErrNoKeys = errors.New("auth: no session signing keys configured")

// NewSigner parses a comma-separated key list. Empty entries are dropped rather
// than accepted as a zero-length key, which would otherwise let an
// almost-unset config produce valid signatures.
func NewSigner(commaSeparated string) (*Signer, error) {
	var keys [][]byte
	for _, k := range strings.Split(commaSeparated, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, []byte(k))
		}
	}
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	return &Signer{keys: keys}, nil
}

// Sign encodes an identity as `payload.mac`, both base64url.
//
// The payload is JSON rather than a delimited string. A field-joined encoding
// ("email|groups|exp") is ambiguous under one MAC: a `|` inside a group name
// shifts the boundaries, so two different identities can share a signature.
func (s *Signer) Sign(id Identity) string {
	id.Expiry = time.Now().Add(sessionTTL).Unix()
	payload, _ := json.Marshal(id)
	b := base64.RawURLEncoding.EncodeToString(payload)
	return b + "." + s.mac(s.keys[0], b)
}

// Verify returns the identity carried by a cookie value, or false.
//
// Every failure is the same `false` — bad format, unknown key, expired. The
// caller has one response to all of them, and distinguishing them for a
// remote party is how signature oracles happen.
func (s *Signer) Verify(value string) (Identity, bool) {
	b, sig, found := strings.Cut(value, ".")
	if !found {
		return Identity{}, false
	}
	var matched bool
	for _, k := range s.keys {
		// hmac.Equal, not ==: a byte-by-byte comparison that returns early
		// leaks where the first difference is.
		if hmac.Equal([]byte(s.mac(k, b)), []byte(sig)) {
			matched = true
			break
		}
	}
	if !matched {
		return Identity{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return Identity{}, false
	}
	var id Identity
	if err := json.Unmarshal(payload, &id); err != nil {
		return Identity{}, false
	}
	if time.Now().Unix() >= id.Expiry {
		return Identity{}, false
	}
	return id, true
}

func (s *Signer) mac(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// setSessionCookie writes the login cookie. Secure and Path=/ are not optional:
// the __Host- prefix makes the browser reject the cookie outright without them,
// which would fail as "login silently does nothing" rather than as an error.
func setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
