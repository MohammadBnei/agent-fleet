package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/defmodels"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/models"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The console's model pick for a definition (docs/adr/0088 amendment). The
// file in git is the default and the bound; the pick is one row per name,
// read when a NEW session is created and when a standing one restarts.
// A running session never changes model.

// defModelStore is *defmodels.Store, as an interface so the rules test against a fake.
type defModelStore interface {
	defmodels.Reader
	List(ctx context.Context) ([]defmodels.Override, error)
	Set(ctx context.Context, definition, model, by string) error
	Clear(ctx context.Context, definition string) error
}

// SetDefinitionModels installs the override store. Without it a definition
// runs on its file's model.default and SetDefinitionModel answers Unavailable.
func (s *Server) SetDefinitionModels(st defModelStore) { s.defModels = st }

// SetGatewayKeyNames installs how core learns which gateway key names the
// provisioner holds (tests; run.go leaves it to the provisioner client).
func (s *Server) SetGatewayKeyNames(f func(ctx context.Context) ([]string, error)) { s.keyNames = f }

func (s *Server) gatewayKeyNames(ctx context.Context) ([]string, error) {
	switch {
	case s.keyNames != nil:
		return s.keyNames(ctx)
	case s.e2e != nil:
		return s.e2e.GatewayKeyNames(ctx)
	}
	return nil, errors.New("no provisioner")
}

// ownKeySuffix is the provisioner's name for a definition's own key
// (provisioner/internal/k8s/credsecret.go gatewayKey).
func ownKeySuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// sharedKeySuffix is the key a gateway definition falls back to.
const sharedKeySuffix = "DEFINITIONS"

// modelProblem is the one rule for whether a definition may be set to a model
// now: "" when it may. The bound is pure (definitions.ModelBound); a gateway id
// also needs the FLEET_GATEWAY_MODELS switch on and a key (its own or the
// shared one) in the provisioner's list. keysErr non-nil means the provisioner
// could not be asked.
func modelProblem(name string, eff *definitions.Spec, model string, gatewayOn bool, keys []string, keysErr error) (reason string, code connect.Code) {
	if r := definitions.ModelBound(eff, model); r != "" {
		return r, connect.CodeInvalidArgument
	}
	if _, gw := models.Gateway(model); !gw {
		return "", 0
	}
	if !gatewayOn {
		return models.DisabledMsg, connect.CodeFailedPrecondition
	}
	if keysErr != nil {
		return "the provisioner cannot be asked which gateway keys exist", connect.CodeUnavailable
	}
	own := ownKeySuffix(name)
	for _, k := range keys {
		if k == own || k == sharedKeySuffix {
			return "", 0
		}
	}
	return fmt.Sprintf("no gateway key: the provisioner has neither LLM_GATEWAY_KEY_FLEET_%s nor LLM_GATEWAY_KEY_FLEET_%s", own, sharedKeySuffix), connect.CodeFailedPrecondition
}

// overrideIgnored says why a stored override is not applied to a new session
// ("" when it is). Only what can change after the write is judged here: the
// bound (the file may have narrowed) and the switch. The key was checked on set.
func (s *Server) overrideIgnored(eff *definitions.Spec, model string) string {
	if r := definitions.ModelBound(eff, model); r != "" {
		return r
	}
	if _, gw := models.Gateway(model); gw && !s.gatewayModels {
		return models.DisabledMsg
	}
	return ""
}

// definitionModel is the model a NEW session of the head gets: a valid override,
// else the file's model.default, else "" (stored as NULL, Claude Code picks). A
// failed read of the override refuses; it never falls back, since a silent
// fallback could put a session on a model the admin had moved it off.
func (s *Server) definitionModel(ctx context.Context, h definitions.Head) (string, error) {
	yaml := defaultModelOf(h.Effective)
	if s.defModels == nil {
		return yaml, nil
	}
	o, err := s.defModels.Get(ctx, h.Name)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard definition model", "definition", h.Name, "error", err)
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("reading the model override of %q: %w", h.Name, err))
	}
	if o == nil {
		return yaml, nil
	}
	if why := s.overrideIgnored(h.Effective, o.Model); why != "" {
		slog.WarnContext(ctx, "definition model override ignored", "definition", h.Name, "model", o.Model, "by", o.SetBy, "reason", why)
		return yaml, nil
	}
	return o.Model, nil
}

// SetDefinitionModel stores, or with model "" clears, the definition's pick.
func (s *Server) SetDefinitionModel(ctx context.Context, req *connect.Request[agentfleetv1.SetDefinitionModelRequest]) (*connect.Response[agentfleetv1.SetDefinitionModelResponse], error) {
	name, model := req.Msg.GetDefinition(), req.Msg.GetModel()
	if s.definitions == nil || s.defModels == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("defined agents are not configured"))
	}
	h, err := s.definitions.Resolve(name)
	switch {
	case errors.Is(err, definitions.ErrUnknown):
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
	case errors.Is(err, definitions.ErrInvalid):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
	case err != nil:
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, err)
	}
	if model == "" {
		if err := s.defModels.Clear(ctx, name); err != nil {
			slog.ErrorContext(ctx, "dashboard SetDefinitionModel", "definition", name, "error", err)
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		slog.InfoContext(ctx, "dashboard SetDefinitionModel", "definition", name, "model", "", "by", who(ctx))
		return connect.NewResponse(&agentfleetv1.SetDefinitionModelResponse{}), nil
	}
	var keys []string
	var keysErr error
	if _, gw := models.Gateway(model); gw && s.gatewayModels {
		keys, keysErr = s.gatewayKeyNames(ctx)
	}
	if why, code := modelProblem(name, h.Effective, model, s.gatewayModels, keys, keysErr); why != "" {
		return nil, errkeys.Connect(code, errkeys.KeyInternal, errors.New(why))
	}
	if err := s.defModels.Set(ctx, name, model, who(ctx)); err != nil {
		slog.ErrorContext(ctx, "dashboard SetDefinitionModel", "definition", name, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard SetDefinitionModel", "definition", name, "model", model, "by", who(ctx))
	return connect.NewResponse(&agentfleetv1.SetDefinitionModelResponse{}), nil
}

// fillDefinitionModel adds the model fields to a listed definition. overrides is
// the store's whole list by name; keys/keysErr are fetched once per list.
func (s *Server) fillDefinitionModel(ctx context.Context, d *agentfleetv1.AgentDefinition, h definitions.Head, overrides map[string]defmodels.Override, keys []string, keysErr error) error {
	d.YamlModel = defaultModelOf(h.Effective)
	d.Model = d.YamlModel
	if o, ok := overrides[h.Name]; ok {
		d.OverrideModel, d.OverrideBy, d.OverrideAt = o.Model, o.SetBy, o.SetAt.UTC().Format(time.RFC3339)
		if d.OverrideIgnored = s.overrideIgnored(h.Effective, o.Model); d.OverrideIgnored == "" {
			d.Model = o.Model
		}
	}
	for _, m := range definitions.ModelCeiling {
		why, _ := modelProblem(h.Name, h.Effective, m, s.gatewayModels, keys, keysErr)
		d.Choices = append(d.Choices, &agentfleetv1.ModelChoice{Model: m, UnavailableReason: why})
	}
	if h.Effective.Lifecycle == nil || !hasStanding(h.Effective) {
		return nil
	}
	d.Standing = true
	row, err := s.sessions.StandingByDefinition(ctx, h.Name)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
	case err != nil:
		return err
	default:
		d.Running = row.StoppedAt == nil
		if row.Model != nil {
			d.RunningModel = *row.Model
		}
	}
	return nil
}

func hasStanding(s *definitions.Spec) bool {
	for _, l := range s.Lifecycle.Allow {
		if l == sessions.Standing {
			return true
		}
	}
	return false
}

// standingRestartModel is the model a restarting standing row is set to, under
// the checks of a create, plus the context-window check of a model switch: a
// smaller window than the transcript already fills would fail on the first message.
func (s *Server) standingRestartModel(ctx context.Context, h definitions.Head, rowID string) (string, error) {
	model, err := s.definitionModel(ctx, h)
	if err != nil {
		return "", err
	}
	if model != "" && !isValidModel(model) {
		return "", errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid model %q", model))
	}
	if err := s.checkModelEnabled(model); err != nil {
		return "", err
	}
	if window, ok := models.ContextWindow(model); ok && s.ratings != nil {
		last, found, err := s.ratings.LastResult(ctx, rowID)
		if err != nil {
			return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		// ContextTokens is 0 on results from before the worker sent it: no check then.
		if found && last.ContextTokens > int64(window) {
			return "", refuse(connect.CodeFailedPrecondition, "the transcript is %d tokens and %s takes %d", last.ContextTokens, model, window)
		}
	}
	return model, nil
}
