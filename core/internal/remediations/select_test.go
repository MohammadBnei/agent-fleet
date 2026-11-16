package remediations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ip(i int) *int { return &i }

func TestParse_RefusesUnknownFieldsAndTrailing(t *testing.T) {
	for _, raw := range []string{`{"action":"restart","name":"a","verb":"exec"}`, `{"action":"restart","name":"a"} {}`, `nope`} {
		if _, err := Parse([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if s, err := Parse([]byte(`{"action":"restart","kind":"deployment","namespace":"x","name":"a"}`)); err != nil || s.Name != "a" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestValidate_SchemaPerAction(t *testing.T) {
	b := Bounds{Min: 1, Max: 3}
	cases := []struct {
		name string
		s    Selection
		ok   bool
	}{
		{"restart", Selection{Action: "restart", Kind: "statefulset", Namespace: "x", Name: "a"}, true},
		{"restart pod kind", Selection{Action: "restart", Kind: "pod", Namespace: "x", Name: "a"}, false},
		{"restart bad ns", Selection{Action: "restart", Kind: "deployment", Namespace: "X_", Name: "a"}, false},
		{"restart with replicas", Selection{Action: "restart", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(1)}, false},
		{"scale in bounds", Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(3)}, true},
		{"scale above", Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(4)}, false},
		{"scale below", Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(0)}, false},
		{"scale none", Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a"}, false},
		{"scale statefulset", Selection{Action: "scale", Kind: "statefulset", Namespace: "x", Name: "a", Replicas: ip(2)}, false},
		{"delete pod", Selection{Action: "delete_pod", Namespace: "x", Name: "a-1"}, true},
		{"delete pod with kind", Selection{Action: "delete_pod", Kind: "deployment", Namespace: "x", Name: "a-1"}, false},
		{"sync", Selection{Action: "sync_app", Name: "game"}, true},
		{"sync with namespace", Selection{Action: "sync_app", Namespace: "x", Name: "game"}, false},
		{"cordon", Selection{Action: "cordon", Name: "node1"}, false},
		{"rollback", Selection{Action: "rollback", Namespace: "x", Name: "a"}, false},
		{"bad name", Selection{Action: "sync_app", Name: "a;rm"}, false},
	}
	for _, c := range cases {
		err := c.s.Validate(b, false)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := (Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(2)}).Validate(Bounds{Min: 1, Max: 99}, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("bounds above the cap: %v", err)
	}
}

func TestValidate_PatchIsLowStakesAndAllowlisted(t *testing.T) {
	b := Bounds{Min: 1, Max: 3}
	p := func(f func(*Selection)) Selection {
		s := Selection{Action: "patch", Kind: "deployment", Namespace: "game", Name: "a", Field: "env", Container: "app", EnvName: "LOG_LEVEL", EnvValue: "debug"}
		f(&s)
		return s
	}
	if err := p(func(*Selection) {}).Validate(b, false); !errors.Is(err, ErrNotLowStakes) {
		t.Fatalf("outside low-stakes: %v", err)
	}
	bad := map[string]Selection{
		"field off the list":  p(func(s *Selection) { s.Field = "image" }),
		"secretish name":      p(func(s *Selection) { s.EnvName = "API_TOKEN" }),
		"secretish value":     p(func(s *Selection) { s.EnvValue = "postgres://u:p@h/db?password=x" }),
		"newline value":       p(func(s *Selection) { s.EnvValue = "a\nb" }),
		"long value":          p(func(s *Selection) { s.EnvValue = strings.Repeat("a", 129) }),
		"replicas over":       p(func(s *Selection) { s.Field, s.Replicas = "replicas", ip(9) }),
		"resources not quant": p(func(s *Selection) { s.Field, s.CPU, s.Memory = "resources", "lots", "1Gi" }),
		"pod kind":            p(func(s *Selection) { s.Kind = "pod" }),
	}
	for name, s := range bad {
		if err := s.Validate(b, true); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	good := []Selection{
		p(func(*Selection) {}),
		p(func(s *Selection) { s.Field, s.Replicas = "replicas", ip(2) }),
		p(func(s *Selection) { s.Field, s.CPU, s.Memory = "resources", "500m", "256Mi" }),
		p(func(s *Selection) { s.Field = "restart" }),
	}
	for i, s := range good {
		if err := s.Validate(b, true); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
}

func TestDenied(t *testing.T) {
	deny := []Selection{
		{Action: "restart", Kind: "deployment", Namespace: "agent-fleet", Name: "core"},
		{Action: "restart", Kind: "deployment", Namespace: "agent-vault", Name: "vault"},
		{Action: "restart", Kind: "deployment", Namespace: "authentik", Name: "server"},
		{Action: "restart", Kind: "statefulset", Namespace: "game", Name: "game-postgres"},
		{Action: "delete_pod", Namespace: "game", Name: "provisioner-abc"},
		{Action: "delete_pod", Namespace: "argocd", Name: "argocd-application-controller-0"},
		{Action: "restart", Kind: "deployment", Namespace: "game", Name: "remediation-executor"},
		{Action: "sync_app", Name: "agent-fleet"},
		{Action: "sync_app", Name: "authentik"},
		{Action: "sync_app", Name: "agent-vault"},
	}
	for _, s := range deny {
		if !s.Denied() {
			t.Errorf("%+v not denied", s)
		}
	}
	for _, s := range []Selection{
		{Action: "restart", Kind: "deployment", Namespace: "ukubi-game", Name: "game"},
		{Action: "sync_app", Name: "ukubi-game"},
	} {
		if s.Denied() {
			t.Errorf("%+v denied", s)
		}
	}
}

type fakeInserter struct{ got []Selection }

func (f *fakeInserter) Insert(_ context.Context, _ string, s Selection, _ Bounds) (string, error) {
	f.got = append(f.got, s)
	return "id1", nil
}

func TestSelect_ChecksBeforeWriting(t *testing.T) {
	list := filepath.Join(t.TempDir(), "low.yaml")
	if err := os.WriteFile(list, []byte("namespaces:\n  - game\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeInserter{}
	svc := &Service{Store: f, LowStakesPath: list}
	b := Bounds{Min: 1, Max: 3}
	ctx := context.Background()

	if id, err := svc.Select(ctx, "r", []byte(`{"action":"restart","kind":"deployment","namespace":"game","name":"a"}`), b); err != nil || id != "id1" {
		t.Fatalf("ok: %q %v", id, err)
	}
	patch := `{"action":"patch","kind":"deployment","namespace":"NS","name":"a","field":"restart"}`
	if _, err := svc.Select(ctx, "r", []byte(strings.Replace(patch, "NS", "game", 1)), b); err != nil {
		t.Fatalf("patch in low-stakes: %v", err)
	}
	if _, err := svc.Select(ctx, "r", []byte(strings.Replace(patch, "NS", "other", 1)), b); !errors.Is(err, ErrNotLowStakes) {
		t.Errorf("patch outside: %v", err)
	}
	// The denylist wins even over a listed namespace.
	if err := os.WriteFile(list, []byte("namespaces:\n  - agent-fleet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(ctx, "r", []byte(strings.Replace(patch, "NS", "agent-fleet", 1)), b); !errors.Is(err, ErrDenied) {
		t.Errorf("denylist beats the list: %v", err)
	}
	if _, err := svc.Select(ctx, "r", []byte(`{"action":"cordon","name":"n"}`), b); !errors.Is(err, ErrInvalid) {
		t.Errorf("cordon: %v", err)
	}
	n := len(f.got)
	// Missing or broken list: nothing is low-stakes.
	for _, path := range []string{"", filepath.Join(t.TempDir(), "absent")} {
		if _, err := (&Service{Store: f, LowStakesPath: path}).Select(ctx, "r", []byte(strings.Replace(patch, "NS", "game", 1)), b); !errors.Is(err, ErrNotLowStakes) {
			t.Errorf("list %q: %v", path, err)
		}
	}
	if len(f.got) != n {
		t.Error("a refused selection was written")
	}
}

type fakePuller struct{ pulls int }

func (f *fakePuller) Pull(context.Context, string) (Row, error) {
	f.pulls++
	return Row{ID: "x", Action: "restart"}, nil
}
func (f *fakePuller) Report(context.Context, string, Outcome) error { return nil }

func TestHandler_BearerOnly(t *testing.T) {
	f := &fakePuller{}
	do := func(h http.Handler, method, tok string) int {
		req := httptest.NewRequest(method, Path+"abc/pull", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	h := Handler(f, "secret")
	if c := do(h, "POST", "wrong"); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := do(h, "POST", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := do(h, "GET", "secret"); c != 401 {
		t.Errorf("GET: %d", c)
	}
	if c := do(Handler(f, ""), "POST", ""); c != 401 {
		t.Errorf("empty configured token: %d", c)
	}
	if f.pulls != 0 {
		t.Fatal("pulled without auth")
	}
	if c := do(h, "POST", "secret"); c != 200 || f.pulls != 1 {
		t.Errorf("right token: %d", c)
	}
}
