package dashboard

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/packs"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Context packs (docs/adr/0063): the console's half of the packs store.
// AdminOnly like the rest of DashboardService until the editor role's ADR.

func (s *Server) packsStore() (*packs.Store, error) {
	if s.packs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("context packs are not configured"))
	}
	return s.packs, nil
}

func packErr(op string, err error) error {
	switch {
	case errors.Is(err, packs.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, packs.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, packs.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	slog.Error("dashboard "+op, "error", err)
	return connect.NewError(connect.CodeInternal, err)
}

// who is the audit name for created_by / attached_by.
func who(ctx context.Context) string {
	if id, ok := auth.FromContext(ctx); ok && id.Email != "" {
		return id.Email
	}
	return "unknown"
}

func (s *Server) ListPacks(ctx context.Context, _ *connect.Request[agentfleetv1.ListPacksRequest]) (*connect.Response[agentfleetv1.ListPacksResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	list, err := ps.List(ctx)
	if err != nil {
		return nil, packErr("ListPacks", err)
	}
	out := &agentfleetv1.ListPacksResponse{}
	for _, p := range list {
		pb := &agentfleetv1.Pack{Name: p.Name, OwnerGroup: p.OwnerGroup, LatestVersion: int32(p.Latest), Text: p.Text}
		for _, sk := range p.Skills {
			pb.Skills = append(pb.Skills, &agentfleetv1.PackSkill{Name: sk.Name, Description: sk.Description, Body: sk.Body})
		}
		for _, a := range p.Attachments {
			pb.Attachments = append(pb.Attachments, &agentfleetv1.PackAttachment{Kind: a.Kind, Target: a.Target})
		}
		out.Packs = append(out.Packs, pb)
	}
	return connect.NewResponse(out), nil
}

func (s *Server) CreatePack(ctx context.Context, req *connect.Request[agentfleetv1.CreatePackRequest]) (*connect.Response[agentfleetv1.CreatePackResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	// ponytail: one owner group until the editor role's ADR scopes them.
	if err := ps.Create(ctx, req.Msg.GetName(), "platform-admins"); err != nil {
		return nil, packErr("CreatePack", err)
	}
	return connect.NewResponse(&agentfleetv1.CreatePackResponse{}), nil
}

func (s *Server) SavePackVersion(ctx context.Context, req *connect.Request[agentfleetv1.SavePackVersionRequest]) (*connect.Response[agentfleetv1.SavePackVersionResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	v := packs.Version{Pack: req.Msg.GetName(), Text: req.Msg.GetText()}
	for _, sk := range req.Msg.GetSkills() {
		v.Skills = append(v.Skills, packs.Skill{Name: sk.GetName(), Description: sk.GetDescription(), Body: sk.GetBody()})
	}
	n, err := ps.SaveVersion(ctx, v, who(ctx))
	if err != nil {
		return nil, packErr("SavePackVersion", err)
	}
	return connect.NewResponse(&agentfleetv1.SavePackVersionResponse{Version: int32(n)}), nil
}

func (s *Server) DeletePack(ctx context.Context, req *connect.Request[agentfleetv1.DeletePackRequest]) (*connect.Response[agentfleetv1.DeletePackResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	if err := ps.Delete(ctx, req.Msg.GetName()); err != nil {
		return nil, packErr("DeletePack", err)
	}
	return connect.NewResponse(&agentfleetv1.DeletePackResponse{}), nil
}

func (s *Server) AttachPack(ctx context.Context, req *connect.Request[agentfleetv1.AttachPackRequest]) (*connect.Response[agentfleetv1.AttachPackResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	a := req.Msg.GetAttachment()
	if err := ps.Attach(ctx, a.GetKind(), a.GetTarget(), req.Msg.GetName(), who(ctx)); err != nil {
		return nil, packErr("AttachPack", err)
	}
	return connect.NewResponse(&agentfleetv1.AttachPackResponse{}), nil
}

func (s *Server) DetachPack(ctx context.Context, req *connect.Request[agentfleetv1.DetachPackRequest]) (*connect.Response[agentfleetv1.DetachPackResponse], error) {
	ps, err := s.packsStore()
	if err != nil {
		return nil, err
	}
	a := req.Msg.GetAttachment()
	if err := ps.Detach(ctx, a.GetKind(), a.GetTarget(), req.Msg.GetName()); err != nil {
		return nil, packErr("DetachPack", err)
	}
	return connect.NewResponse(&agentfleetv1.DetachPackResponse{}), nil
}
