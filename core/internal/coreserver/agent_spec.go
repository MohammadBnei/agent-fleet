package coreserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

var errSpecNotApplied = status.Error(codes.FailedPrecondition, "worker has not applied the definition spec (docs/adr/0066 §8)")

// agentSpecOf is a session's AgentSpec and its sha, both nil/"" for a repo
// session (docs/adr/0067 §4). Built from the pinned row on every call, never
// cached: the sha GetSession hands out and the one the gate below compares
// against are then the same computation, and a restarted core needs no state.
func (s *Server) agentSpecOf(ctx context.Context, sessionID string) (*agentfleetv1.AgentSpec, string, error) {
	raw, lifecycle, err := s.sessions.PinnedEffective(ctx, sessionID)
	switch {
	case errors.Is(err, sessions.ErrDefinitionInvalid):
		return nil, "", status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, sessions.ErrNotFound):
		return nil, "", status.Errorf(codes.NotFound, "session %s not found", sessionID)
	case err != nil:
		return nil, "", err
	case raw == nil:
		return nil, "", nil
	}
	var eff definitions.Spec
	if err := json.Unmarshal(raw, &eff); err != nil {
		return nil, "", fmt.Errorf("definition spec of %s: %w", sessionID, err)
	}
	spec, err := agentSpec(&eff, lifecycle)
	if err != nil {
		return nil, "", err
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

// agentSpec flattens an effective spec into what the worker applies.
// Interaction stays "": it left the spec (ADR-0070 §2). Lifecycle is the
// session row's, passed in, and is sent for single-shot only (with the
// definition's output schema): a resident session keeps both empty, which
// proto's deterministic encoding omits, so its agent_spec_sha is the one it
// always had (ADR-0070 §3).
func agentSpec(eff *definitions.Spec, lifecycle string) (*agentfleetv1.AgentSpec, error) {
	a := &agentfleetv1.AgentSpec{SidecarTools: eff.SidecarTools}
	if t := eff.Tools; t != nil {
		a.ToolsAllow, a.ToolsDeny = t.Allow, t.Deny
	}
	if m := eff.Model; m != nil {
		a.ModelAllow = m.Allow
	}
	if t := eff.Thinking; t != nil {
		a.ThinkingDefault = t.Default
	}
	if m := eff.Mode; m != nil {
		a.ModeAllow = m.Allow
	}
	if p := eff.SystemPrompt; p != nil {
		a.SystemPromptAppend = p.Append
	}
	if l := eff.Limits; l != nil {
		a.MaxTurns = int32(l.MaxTurns)
	}
	// docs/adr/0068 §4: a workspace an island host serves gets the
	// script-write ask in the worker.
	a.ScriptAsk = eff.Embed != nil && len(eff.Embed.Origins) > 0
	if lifecycle == "single-shot" {
		a.Lifecycle = lifecycle
		schema, err := definitions.OutputSchemaJSON(eff)
		if err != nil {
			return nil, err
		}
		a.OutputSchema = schema
	}
	return a, nil
}

// requireSpecApplied is the gate of docs/adr/0066 §8: a definition session
// gets no transcript entry until its worker echoes the sha of the spec core
// would hand it NOW. Stateless, so it survives a core restart, and an older
// worker, which sends no sha, receives nothing whoever warmed it.
//
// Keyed on the session rather than the caller: auth.go binds both methods to
// the caller's own session, so a definition session is only ever reached by
// its definition lease, and a stray path that skipped the interceptor still
// fails closed. A repo session costs one row read per call.
func (s *Server) requireSpecApplied(ctx context.Context, sessionID, applied string) error {
	// ponytail: a Server built without a session store (the transcript-only
	// unit tests) has no pinned definition row to gate against. cmd/core and
	// the auth interceptor always have one.
	if s.sessions == nil {
		return nil
	}
	_, want, err := s.agentSpecOf(ctx, sessionID)
	if err != nil {
		return err
	}
	if want != "" && applied != want {
		return errSpecNotApplied
	}
	return nil
}
