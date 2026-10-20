package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves /app/installations and access_tokens, records each mint.
type fakeGitHub struct {
	t       *testing.T
	pub     *rsa.PublicKey
	mu      sync.Mutex
	mints   []map[string]any
	expires time.Time
	fail    bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.checkJWT(r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		switch strings.ToLower(r.URL.Path) {
		case "/repos/mohammadbnei/notes/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "account": map[string]any{"id": 43962695, "login": "MohammadBnei"}})
		case "/repos/mohammadbnei/squatted/installation": // same login, foreign account id
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "account": map[string]any{"id": 666, "login": "MohammadBnei"}})
		case "/repos/opus-tech-hq/workspace/installation", "/repos/opus-tech-hq/other/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 3, "account": map[string]any{"id": 344349211, "login": "opus-tech-hq"}})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["path"] = r.URL.Path
		f.mints = append(f.mints, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_" + string(rune('a'+len(f.mints))), "expires_at": f.expires.Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) checkJWT(h string) {
	parts := strings.Split(strings.TrimPrefix(h, "Bearer "), ".")
	if len(parts) != 3 {
		f.t.Errorf("not a JWT: %q", h)
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		f.t.Errorf("JWT signature: %v", err)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	// iat backdated, exp within GitHub's 10m window (relative: tests shift now).
	if claims.Iss != "5395733" || claims.Exp-claims.Iat > 660 || claims.Exp-claims.Iat < 540 {
		f.t.Errorf("claims %+v", claims)
	}
}

func newApp(t *testing.T, pkcs8 bool) (*App, *fakeGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, typ := x509.MarshalPKCS1PrivateKey(key), "RSA PRIVATE KEY"
	if pkcs8 {
		der, _ = x509.MarshalPKCS8PrivateKey(key)
		typ = "PRIVATE KEY"
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "GITHUB_APP_ID"), []byte("5395733\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "GITHUB_APP_PRIVATE_KEY"), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
	f := &fakeGitHub{t: t, pub: &key.PublicKey, expires: time.Now().Add(time.Hour)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	a := NewApp(dir, []int64{43962695, 344349211})
	a.API = srv.URL
	return a, f
}

func TestApp_MintsOneRepoTokenFromTheAllowlistedInstallation(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		a, f := newApp(t, pkcs8)
		tok, err := a.Token(context.Background(), "mohammadbnei", "notes")
		if err != nil || tok == "" {
			t.Fatalf("pkcs8=%v: %q %v", pkcs8, tok, err)
		}
		m := f.mints[0]
		if m["path"] != "/app/installations/2/access_tokens" {
			t.Errorf("minted on %v, want the allowlisted installation 2", m["path"])
		}
		if repos, _ := m["repositories"].([]any); len(repos) != 1 || repos[0] != "notes" {
			t.Errorf("repositories %v", m["repositories"])
		}
		perms, _ := m["permissions"].(map[string]any)
		if perms["contents"] != "write" || perms["pull_requests"] != "write" || perms["metadata"] != "read" || perms["workflows"] != nil || len(perms) != 3 {
			t.Errorf("permissions %v", perms)
		}
	}
}

func TestApp_RefusesAnInstallationByANonAllowlistedAccount(t *testing.T) {
	a, f := newApp(t, false)
	if _, err := a.Token(context.Background(), "MohammadBnei", "squatted"); err == nil {
		t.Fatal("want an error: the repo's installation is by a foreign account id")
	}
	if len(f.mints) != 0 {
		t.Errorf("minted %d tokens", len(f.mints))
	}
}

func TestApp_ReusesUntilThirtyMinutesRemain(t *testing.T) {
	a, f := newApp(t, false)
	ctx := context.Background()
	t1, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	t2, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	if t1 != t2 || len(f.mints) != 1 {
		t.Fatalf("a fresh token was re-minted: %d mints", len(f.mints))
	}
	a.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	t3, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	if t3 == t1 || len(f.mints) != 2 {
		t.Fatalf("a token with <30m left was served: %d mints", len(f.mints))
	}
}

// A failed re-mint serves the last token, even expired: the vault keeps what
// it has instead of losing a service (docs/adr/0075 §5).
func TestApp_FailedRemintServesTheLastToken(t *testing.T) {
	a, f := newApp(t, false)
	ctx := context.Background()
	t1, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	f.fail = true
	a.now = func() time.Time { return time.Now().Add(45 * time.Minute) } // <30m left, still valid
	t2, err := a.Token(ctx, "opus-tech-hq", "workspace")
	if err != nil || t2 != t1 {
		t.Fatalf("got %q %v, want the last token", t2, err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // expired
	if _, err := a.Token(ctx, "opus-tech-hq", "workspace"); err == nil {
		t.Fatal("an expired token was served as valid")
	}
	if _, err := a.Token(ctx, "opus-tech-hq", "other"); err == nil {
		t.Fatal("no token yet and the mint fails: want an error")
	}
}

func TestApp_ConfiguredOnlyWithBothFiles(t *testing.T) {
	if NewApp(t.TempDir(), nil).Configured() {
		t.Error("empty dir reads as configured")
	}
	a, _ := newApp(t, false)
	if !a.Configured() {
		t.Error("both files present, not configured")
	}
}
