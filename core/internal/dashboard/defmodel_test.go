package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/defmodels"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

type fakeDefModels struct {
	o   *defmodels.Override
	err error
}

func (f fakeDefModels) Get(context.Context, string) (*defmodels.Override, error) { return f.o, f.err }
func (fakeDefModels) List(context.Context) ([]defmodels.Override, error)         { return nil, nil }
func (fakeDefModels) Set(context.Context, string, string, string) error           { return nil }
func (fakeDefModels) Clear(context.Context, string) error                         { return nil }

func head(name string, m *definitions.Model) definitions.Head {
	return definitions.Head{Name: name, Effective: &definitions.Spec{Model: m}}
}

func TestDefinitionModel(t *testing.T) {
	claude := head("dev", &definitions.Model{Default: "claude-sonnet-5", Allow: []string{"claude-sonnet-5", "claude-opus-5"}})
	alert := head("alert-responder", &definitions.Model{Default: "z-ai-glm-5-3", Allow: []string{"z-ai-glm-5-3"}})
	noModel := head("free", nil)
	ov := func(m string) *defmodels.Override { return &defmodels.Override{Model: m, SetBy: "root"} }
	for _, tc := range []struct {
		name      string
		h         definitions.Head
		store     defModelStore
		gateway   bool
		want      string
		wantError bool
	}{
		{"no store: the file", claude, nil, false, "claude-sonnet-5", false},
		{"no override: the file", claude, fakeDefModels{}, false, "claude-sonnet-5", false},
		{"valid override wins", claude, fakeDefModels{o: ov("claude-opus-5")}, false, "claude-opus-5", false},
		{"override outside model.allow is ignored", claude, fakeDefModels{o: ov("claude-haiku-4-5")}, false, "claude-sonnet-5", false},
		{"alert-responder ignores any other id", alert, fakeDefModels{o: ov("claude-opus-5")}, true, "z-ai-glm-5-3", false},
		{"override outside the ceiling is ignored", noModel, fakeDefModels{o: ov("claude-opus-4-8")}, false, "", false},
		{"no model set, override in the ceiling", noModel, fakeDefModels{o: ov("claude-sonnet-5")}, false, "claude-sonnet-5", false},
		{"gateway override with the switch off is ignored", noModel, fakeDefModels{o: ov("kimi-k2-7-code")}, false, "", false},
		{"gateway override with the switch on", noModel, fakeDefModels{o: ov("kimi-k2-7-code")}, true, "kimi-k2-7-code", false},
		{"a DB error refuses, never falls back", claude, fakeDefModels{err: errors.New("db down")}, false, "", true},
	} {
		s := &Server{defModels: tc.store, gatewayModels: tc.gateway}
		got, err := s.definitionModel(context.Background(), tc.h)
		if (err != nil) != tc.wantError || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q (error %v)", tc.name, got, err, tc.want, tc.wantError)
		}
	}
}

func TestModelProblem(t *testing.T) {
	alert := &definitions.Spec{Model: &definitions.Model{Default: "z-ai-glm-5-3", Allow: []string{"z-ai-glm-5-3"}}}
	free := &definitions.Spec{}
	down := errors.New("down")
	for _, tc := range []struct {
		name      string
		eff       *definitions.Spec
		def, m    string
		gateway   bool
		keys      []string
		keysErr   error
		wantCode  connect.Code
		wantInMsg string
	}{
		{"alert-responder refuses another id", alert, "alert-responder", "kimi-k2-7-code", true, []string{"DEFINITIONS"}, nil, connect.CodeInvalidArgument, "model.allow"},
		{"alert-responder refuses a claude id", alert, "alert-responder", "claude-opus-5", true, nil, nil, connect.CodeInvalidArgument, "model.allow"},
		{"alert-responder takes its own with its own key", alert, "alert-responder", "z-ai-glm-5-3", true, []string{"ALERT_RESPONDER"}, nil, 0, ""},
		{"alert-responder takes its own with the shared key", alert, "alert-responder", "z-ai-glm-5-3", true, []string{"DEFINITIONS"}, nil, 0, ""},
		{"switch off", free, "x", "kimi-k2-7-code", false, []string{"X"}, nil, connect.CodeFailedPrecondition, "FLEET_GATEWAY_MODELS"},
		{"no key names both", free, "my-def", "kimi-k2-7-code", true, []string{"CONSOLE"}, nil, connect.CodeFailedPrecondition, "LLM_GATEWAY_KEY_FLEET_MY_DEF"},
		{"the console key is not a definition's", free, "my-def", "kimi-k2-7-code", true, []string{"CONSOLE"}, nil, connect.CodeFailedPrecondition, "LLM_GATEWAY_KEY_FLEET_DEFINITIONS"},
		{"provisioner cannot be asked", free, "my-def", "kimi-k2-7-code", true, nil, down, connect.CodeUnavailable, "cannot be asked"},
		{"a claude id needs no key", free, "my-def", "claude-sonnet-5", false, nil, nil, 0, ""},
	} {
		why, code := modelProblem(tc.def, tc.eff, tc.m, tc.gateway, tc.keys, tc.keysErr)
		if (why == "") != (tc.wantInMsg == "") || code != tc.wantCode || !strings.Contains(why, tc.wantInMsg) {
			t.Errorf("%s: %q (%v), want code %v containing %q", tc.name, why, code, tc.wantCode, tc.wantInMsg)
		}
	}
}
