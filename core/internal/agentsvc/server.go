package agentsvc

import (
	"context"
	"slices"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// Server implements AgentService. Methods not implemented yet answer
// Unimplemented through the embedded handler, behind the authz table.
type Server struct {
	agentfleetv1connect.UnimplementedAgentServiceHandler
	definitions heads
	dash        *dashboard.Server
	sessions    *sessions.Store
	transcr     transcript.Store
	hub         *dashboard.Hub
}

// heads is the part of *definitions.Registry this reads.
type heads interface {
	List() ([]definitions.Head, []string)
	Resolve(name string) (definitions.Head, error)
}

var _ agentfleetv1connect.AgentServiceHandler = (*Server)(nil)

// NewServer reuses the console's server for creates and warms, so every pod
// still goes through WarmIfIdle (docs/adr/0066 §8).
func NewServer(defs *definitions.Registry, dash *dashboard.Server, store *sessions.Store, transcr transcript.Store, hub *dashboard.Hub) *Server {
	s := &Server{dash: dash, sessions: store, transcr: transcr, hub: hub}
	if defs != nil {
		s.definitions = defs
	}
	return s
}

func principal(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, connect.NewError(connect.CodeUnauthenticated, errNoPrincipal)
	}
	return p, nil
}

// mayUse is docs/adr/0066 §5 for a member, judged on the current head: in the
// audience group, or an admin.
func mayUse(p Principal, spec *definitions.Spec) bool {
	if p.Admin() {
		return true
	}
	return p.Member != nil && spec.Audience != nil && p.Member.InGroup(spec.Audience.Group)
}

// ListDefinitions lists the valid heads the caller may use. Clients get none
// until the client principal lands (docs/adr/0066 §3).
func (s *Server) ListDefinitions(ctx context.Context, _ *connect.Request[agentfleetv1.AgentServiceListDefinitionsRequest]) (*connect.Response[agentfleetv1.AgentServiceListDefinitionsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	out := &agentfleetv1.AgentServiceListDefinitionsResponse{}
	if s.definitions == nil || p.Member == nil {
		return connect.NewResponse(out), nil
	}
	listed, _ := s.definitions.List()
	for _, l := range listed {
		// Resolve, not the listed head: it fails closed before the first
		// sync and once the last good one is stale, as creating does.
		h, err := s.definitions.Resolve(l.Name)
		if err != nil || !mayUse(p, h.Effective) {
			continue
		}
		e := h.Effective
		d := &agentfleetv1.AgentDefinitionInfo{Name: h.Name}
		if e.Model != nil {
			d.ModelAllow = slices.Clone(e.Model.Allow)
		}
		if e.Mode != nil {
			d.ModeAllow = slices.Clone(e.Mode.Allow)
		}
		if e.Interaction != nil {
			d.InteractionAllow = slices.Clone(e.Interaction.Allow)
		}
		if e.Lifecycle != nil {
			d.LifecycleAllow = slices.Clone(e.Lifecycle.Allow)
		}
		out.Definitions = append(out.Definitions, d)
	}
	return connect.NewResponse(out), nil
}
