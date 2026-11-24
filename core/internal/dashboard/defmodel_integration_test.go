//go:build integration

package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/defmodels"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func adminCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{Sub: "root", Email: "root@example.invalid", Groups: []string{auth.RequiredGroup}})
}

func setModel(s *Server, def, model string) error {
	_, err := s.SetDefinitionModel(adminCtx(), connect.NewRequest(&agentfleetv1.SetDefinitionModelRequest{Definition: def, Model: model}))
	return err
}

// The console write: bound, switch and key checks, then store, list and clear.
func TestSetDefinitionModel(t *testing.T) {
	s, _, _, _, pool := newDefinitionsServerPool(t)
	st := defmodels.NewStore(pool)
	s.SetDefinitionModels(st)
	keys := []string{"CONSOLE"}
	var keysErr error
	s.SetGatewayKeyNames(func(context.Context) ([]string, error) { return keys, keysErr })
	ctx := adminCtx()

	// alert-responder (allow [z-ai-glm-5-3]) refuses every other id.
	for _, m := range []string{"claude-opus-5", "kimi-k2-7-code", "not-a-model"} {
		s.SetGatewayModels(true)
		if err := setModel(s, "alert-responder", m); code(err) != connect.CodeInvalidArgument {
			t.Errorf("alert-responder %s: %v", m, err)
		}
	}
	// Its own id: the switch, then the key, naming what is missing, then the provisioner's availability.
	s.SetGatewayModels(false)
	if err := setModel(s, "alert-responder", "z-ai-glm-5-3"); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "FLEET_GATEWAY_MODELS") {
		t.Fatalf("switch off: %v", err)
	}
	s.SetGatewayModels(true)
	if err := setModel(s, "alert-responder", "z-ai-glm-5-3"); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "LLM_GATEWAY_KEY_FLEET_ALERT_RESPONDER") {
		t.Fatalf("no key: %v", err)
	}
	keysErr = errors.New("down")
	if err := setModel(s, "alert-responder", "z-ai-glm-5-3"); code(err) != connect.CodeUnavailable {
		t.Fatalf("provisioner down: %v", err)
	}
	keysErr, keys = nil, []string{"DEFINITIONS"}
	if err := setModel(s, "alert-responder", "z-ai-glm-5-3"); err != nil {
		t.Fatalf("shared key: %v", err)
	}
	if err := setModel(s, "nope", "claude-sonnet-5"); code(err) != connect.CodeNotFound {
		t.Fatalf("unknown definition: %v", err)
	}

	// A Claude id needs no key and is judged by the ceiling only on a definition with no model.
	if err := setModel(s, "dev-dream-analyst", "claude-opus-4-8"); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("outside the ceiling: %v", err)
	}
	if err := setModel(s, "dev-dream-analyst", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	o, _ := st.Get(ctx, "dev-dream-analyst")
	if o == nil || o.Model != "claude-sonnet-5" || o.SetBy != "root@example.invalid" {
		t.Fatalf("stored %+v", o)
	}
	res, err := s.ListDefinitions(ctx, connect.NewRequest(&agentfleetv1.ListDefinitionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Msg.GetDefinitions() {
		if d.GetName() == "dev-dream-analyst" {
			if d.GetModel() != "claude-sonnet-5" || d.GetOverrideModel() != "claude-sonnet-5" || d.GetOverrideBy() != "root@example.invalid" || d.GetYamlModel() != "" || len(d.GetChoices()) == 0 {
				t.Errorf("listed %v", d)
			}
			for _, c := range d.GetChoices() {
				// The shared key exists, so a free definition may pick a gateway id.
				if c.GetModel() == "kimi-k2-7-code" && c.GetUnavailableReason() != "" {
					t.Errorf("kimi unavailable: %q", c.GetUnavailableReason())
				}
				if c.GetModel() == "claude-opus-4-8" {
					t.Error("a model outside the ceiling is offered")
				}
			}
		}
		if d.GetName() == "alert-responder" {
			for _, c := range d.GetChoices() {
				if (c.GetModel() == "z-ai-glm-5-3") != (c.GetUnavailableReason() == "") {
					t.Errorf("alert-responder choice %v", c)
				}
			}
		}
	}
	if err := setModel(s, "dev-dream-analyst", ""); err != nil {
		t.Fatal(err)
	}
	if o, _ := st.Get(ctx, "dev-dream-analyst"); o != nil {
		t.Fatalf("not cleared: %+v", o)
	}
}

// A new session takes a valid override; an out-of-bound stored one is ignored
// and listed with its reason; a failing read refuses the create.
func TestCreateSession_DefinitionModelOverride(t *testing.T) {
	s, _, _, store, pool := newDefinitionsServerPool(t)
	st := defmodels.NewStore(pool)
	s.SetDefinitionModels(st)
	ctx := context.Background()

	if err := st.Set(ctx, "dev-dream-analyst", "claude-sonnet-5", "root"); err != nil {
		t.Fatal(err)
	}
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil || sess.GetModel() != "claude-sonnet-5" {
		t.Fatalf("override: %v, %v", sess.GetModel(), err)
	}
	// The ceiling later drops the id: ignored, so NULL as today, and shown with its reason.
	if err := st.Set(ctx, "dev-dream-analyst", "claude-opus-4-8", "root"); err != nil {
		t.Fatal(err)
	}
	sess2, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil || sess2.Model != nil {
		t.Fatalf("out-of-bound override: %v, %v", sess2.GetModel(), err)
	}
	if got, _ := store.Get(ctx, sess.GetId()); got == nil || got.Model == nil || *got.Model != "claude-sonnet-5" {
		t.Fatal("an existing session lost its model")
	}
	res, err := s.ListDefinitions(ctx, connect.NewRequest(&agentfleetv1.ListDefinitionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Msg.GetDefinitions() {
		if d.GetName() == "dev-dream-analyst" && (d.GetOverrideIgnored() == "" || d.GetModel() != "") {
			t.Errorf("listed %v", d)
		}
	}
	// A read error refuses; it never falls back.
	s.SetDefinitionModels(fakeDefModels{err: errors.New("db down")})
	if _, err := createDef(s, "dev-dream-analyst", nil); err == nil {
		t.Fatal("a create went ahead on a failed override read")
	}
}

// The shared key is requested only for a session whose model is a gateway id.
func TestWarm_SharedGatewayKeyOnlyForGatewayModels(t *testing.T) {
	s, prov, _, _, pool := newDefinitionsServerPool(t)
	st := defmodels.NewStore(pool)
	s.SetDefinitionModels(st)
	s.SetGatewayModels(true)
	ctx := context.Background()
	for i, tc := range []struct {
		model string
		want  bool
	}{{"claude-sonnet-5", false}, {"kimi-k2-7-code", true}} {
		if err := st.Set(ctx, "dev-dream-analyst", tc.model, "root"); err != nil {
			t.Fatal(err)
		}
		sess, err := createDef(s, "dev-dream-analyst", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.WarmIfIdle(ctx, sess.GetId()); err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		prov.mu.Lock()
		got := prov.creates[i].GetDefinitionSharedGatewayKey()
		prov.mu.Unlock()
		if got != tc.want {
			t.Errorf("%s: shared key asked = %v, want %v", tc.model, got, tc.want)
		}
		// Free the definition pool slot for the next session.
		if _, err := s.sessions.Get(ctx, sess.GetId()); err != nil {
			t.Fatal(err)
		}
		if err := s.sessions.SetPodPhase(ctx, sess.GetId(), agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), ""); err != nil {
			t.Fatal(err)
		}
	}
}

// A stopped standing character takes the current effective model on Start; a running one keeps its own.
func TestStandingRestartTakesTheEffectiveModel(t *testing.T) {
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	st := defmodels.NewStore(pool)
	s.SetDefinitionModels(st)
	ctx := adminCtx()
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-stander.yaml"] = []byte(strings.NewReplacer("name: dev-dream-analyst }", "name: dev-stander }",
			"vault: dev-dream-analyst", "vault: dev-stander").Replace(string(p.files["definitions/dev-dream-analyst.yaml"])) +
			"  lifecycle: { allow: [standing] }\n")
	})
	if err := reg.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	s.caps.MaxStanding = 2
	start := func() (*agentfleetv1.Session, error) {
		res, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: "dev-stander"}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetSession(), nil
	}
	first, err := start()
	if err != nil || first.Model != nil {
		t.Fatalf("first start: %v, %v", first.GetModel(), err)
	}
	if err := st.Set(ctx, "dev-stander", "claude-haiku-4-5", "root"); err != nil {
		t.Fatal(err)
	}
	// Running: the row keeps its model, and the list says so.
	res, err := s.ListDefinitions(ctx, connect.NewRequest(&agentfleetv1.ListDefinitionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Msg.GetDefinitions() {
		if d.GetName() == "dev-stander" && (!d.GetStanding() || !d.GetRunning() || d.GetRunningModel() != "" || d.GetModel() != "claude-haiku-4-5") {
			t.Errorf("listed %v", d)
		}
	}
	if got, _ := store.Get(ctx, first.GetId()); got.Model != nil {
		t.Fatal("a running row changed model")
	}
	if _, err := s.StopStanding(ctx, connect.NewRequest(&agentfleetv1.StopStandingRequest{Definition: "dev-stander"})); err != nil {
		t.Fatal(err)
	}
	// A DB error on the override read refuses the restart.
	s.SetDefinitionModels(fakeDefModels{err: errors.New("db down")})
	if _, err := start(); err == nil {
		t.Fatal("restart went ahead on a failed override read")
	}
	s.SetDefinitionModels(st)
	again, err := start()
	if err != nil || again.GetModel() != "claude-haiku-4-5" {
		t.Fatalf("restart: %v, %v", again.GetModel(), err)
	}
}
