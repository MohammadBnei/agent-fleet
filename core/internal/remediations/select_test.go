package remediations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
		{"dotted namespace", Selection{Action: "restart", Kind: "deployment", Namespace: "a.b", Name: "a"}, false},
	}
	for _, c := range cases {
		err := c.s.Validate(b)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := (Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(2)}).Validate(Bounds{Min: 1, Max: 99}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bounds above the cap: %v", err)
	}
}

func TestValidate_UnsetBoundsRefuseReplicas(t *testing.T) {
	scale := Selection{Action: "scale", Kind: "deployment", Namespace: "x", Name: "a", Replicas: ip(0)}
	for _, b := range []Bounds{{}, {Min: 0, Max: 3}} {
		if err := scale.Validate(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("scale to zero with %+v: %v", b, err)
		}
	}
	one := 1
	scale.Replicas = &one
	if scale.Validate(Bounds{}) == nil {
		t.Error("unset bounds admitted replicas")
	}
}

func TestValidate_PatchIsNotInTheCatalogue(t *testing.T) {
	s := Selection{Action: "patch", Kind: "deployment", Namespace: "game", Name: "a"}
	if err := s.Validate(Bounds{Min: 1, Max: 3}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("patch: %v", err)
	}
	if _, err := Parse([]byte(`{"action":"restart","kind":"deployment","namespace":"game","name":"a","field":"env"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a patch-only field: %v", err)
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
		{Action: "sync_app", Name: "agent-fleet-core"},
		{Action: "sync_app", Name: "platform-provisioner"},
		{Action: "sync_app", Name: "platform-thot"},
		{Action: "sync_app", Name: "bootstrap"},
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
	f := &fakeInserter{}
	svc := &Service{Store: f}
	b := Bounds{Min: 1, Max: 3}
	ctx := context.Background()

	if id, err := svc.Select(ctx, "r", []byte(`{"action":"restart","kind":"deployment","namespace":"game","name":"a"}`), b); err != nil || id != "id1" {
		t.Fatalf("ok: %q %v", id, err)
	}
	patch := `{"action":"patch","kind":"deployment","namespace":"NS","name":"a"}`
	if _, err := svc.Select(ctx, "r", []byte(strings.Replace(patch, "NS", "game", 1)), b); !errors.Is(err, ErrInvalid) {
		t.Errorf("patch: %v", err)
	}
	if _, err := svc.Select(ctx, "r", []byte(`{"action":"restart","kind":"deployment","namespace":"agent-fleet","name":"a"}`), b); !errors.Is(err, ErrDenied) {
		t.Errorf("denylist: %v", err)
	}
	if _, err := svc.Select(ctx, "r", []byte(`{"action":"cordon","name":"n"}`), b); !errors.Is(err, ErrInvalid) {
		t.Errorf("cordon: %v", err)
	}
	if len(f.got) != 1 {
		t.Errorf("a refused selection was written: %d rows", len(f.got))
	}
}

type fakePuller struct{ pulls int }

func (f *fakePuller) Pull(context.Context, string) (Row, error) {
	f.pulls++
	return Row{ID: "x", Action: "restart"}, nil
}
func (f *fakePuller) Report(context.Context, string, Outcome) error { return nil }

func TestHandler_MalformedIDIs404(t *testing.T) {
	f := &fakePuller{}
	for _, op := range []string{"pull", "report"} {
		req := httptest.NewRequest("POST", Path+"not-a-uuid/"+op, strings.NewReader(`{"status":"done"}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		Handler(f, "secret").ServeHTTP(rec, req)
		if rec.Code != 404 || f.pulls != 0 {
			t.Errorf("%s: %d", op, rec.Code)
		}
	}
}

func TestHandler_BearerOnly(t *testing.T) {
	f := &fakePuller{}
	do := func(h http.Handler, method, tok string) int {
		req := httptest.NewRequest(method, Path+"0b5f8c1e-2f3a-4c55-9a52-1d3a7c2f8e10/pull", nil)
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
