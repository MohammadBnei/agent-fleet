package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	"encoding/json"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// CardEffects is what the core-run card verbs reach (docs/adr/0086 §4). A nil field refuses its verb.
type CardEffects struct {
	// Remediate is remediations.Service.Select: the schema, denylist and rate limits, then a pending row.
	Remediate func(ctx context.Context, responder string, raw []byte, b remediations.Bounds) (string, error)
	// Check is remediations.Service.Check: Select's refusals and rate limit, nothing written.
	Check func(ctx context.Context, responder string, raw []byte, b remediations.Bounds) error
	// OpenPR is the provisioner's OpenPullRequest.
	OpenPR func(ctx context.Context, req *agentfleetv1.OpenPullRequestRequest) (*agentfleetv1.OpenPullRequestResponse, error)
}

// SetCardEffects wires the catalogue and open_pr verbs.
func (s *Server) SetCardEffects(fx CardEffects) { s.cardFx = fx }

// sessionOpener starts open_session's session (*dashboard.Server).
type sessionOpener interface {
	StartableRepo(ctx context.Context, name string) (*repos.Repo, error)
	OpenSupervised(ctx context.Context, repo, title, body string) (string, error)
}

// coreEffect is the chosen option's effect when it is a core-run verb.
func coreEffect(r decisions.Record, optionID string) *agentfleetv1.CardEffect {
	for _, o := range cardOf(r).GetOptions() {
		if o.GetId() == optionID && decisions.IsCoreVerb(o.GetEffect().GetVerb()) {
			return o.GetEffect()
		}
	}
	return nil
}

func refuse(err error) error {
	return errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
}

// checkEffect runs before the answer is recorded: a refusal leaves the card open. The approval binds to
// the payload hash, and open_pr's writer must still be a B.C session.
func (s *Server) checkEffect(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect) error {
	if e.GetVerb() == decisions.VerbExecutorStep {
		return refuse(errors.New("executor_step is declared but refused until ADR-0072's executor exists"))
	}
	if err := decisions.VerifyEffect(e); err != nil {
		return errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyDecisionStale, err)
	}
	if e.GetVerb() == decisions.VerbOpenPR {
		return s.writerIsBC(ctx, r, e)
	}
	return nil
}

// preflight is what can fail in the effect, checked before the answer is recorded so a refusal spends no
// approval: the catalogue's own checks and rate limit, the repo startable, the PR's repo the writer's own.
func (s *Server) preflight(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect) error {
	switch e.GetVerb() {
	case decisions.VerbCatalogue:
		if s.cardFx.Check != nil {
			if err := s.cardFx.Check(ctx, cardOf(r).GetWriterDefinition(), []byte(e.GetPayload()), s.boundsOf(ctx, r)); err != nil {
				return refuse(err)
			}
		}
	case decisions.VerbOpenSession:
		var b decisions.Brief
		if err := json.Unmarshal([]byte(e.GetPayload()), &b); err != nil {
			return refuse(err)
		}
		if s.opener != nil {
			if _, err := s.opener.StartableRepo(ctx, b.Repo); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) boundsOf(ctx context.Context, r decisions.Record) (b remediations.Bounds) {
	if raw, _, err := s.sessions.PinnedEffective(ctx, r.SessionID); err == nil {
		var eff definitions.Spec
		if json.Unmarshal(raw, &eff) == nil && eff.Trust != nil && eff.Trust.Scale != nil {
			b = remediations.Bounds{Min: eff.Trust.Scale.Min, Max: eff.Trust.Scale.Max}
		}
	}
	return b
}

// writerIsBC: open_pr's writer is still a B.C session, and the PR's repo is the repo it works on
// (a branch pushed by a B.C session, docs/adr/0086 §4), so a filer's bug cannot open one elsewhere.
func (s *Server) writerIsBC(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect) error {
	w, err := s.sessions.Get(ctx, r.SessionID)
	if err != nil || w == nil {
		return refuse(fmt.Errorf("open_pr: the writer %s is gone", r.SessionID))
	}
	if w.TrustSet != "BC" {
		return refuse(fmt.Errorf("open_pr needs a B.C writer, this one holds %q", w.TrustSet))
	}
	var t decisions.PRTarget
	if err := json.Unmarshal([]byte(e.GetPayload()), &t); err != nil {
		return refuse(err)
	}
	if s.opener == nil {
		return refuse(errors.New("open_pr: no console"))
	}
	rp, err := s.opener.StartableRepo(ctx, w.Repo)
	if err != nil {
		return err
	}
	if u := strings.TrimSuffix(rp.URL, ".git"); !strings.HasSuffix(u, "/"+t.Repo) && !strings.HasSuffix(u, ":"+t.Repo) {
		return refuse(fmt.Errorf("open_pr: %s is not the repo the writer works on", t.Repo))
	}
	return nil
}

// effectFailed records on the card's thread that an answered card's effect did not run (the card is
// closed, so the owner cannot retry it: core files a new one), so the failure is on the card and not
// only in the RPC error.
func (s *Server) effectFailed(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect, cause error) {
	note := fmt.Sprintf("The owner answered card %s, but its %s effect did not run: %v", r.ID, e.GetVerb(), cause)
	if _, err := s.transcr.Append(ctx, r.SessionID, "system", note, "discussion", "effect-failed/"+r.ID); err != nil {
		slog.ErrorContext(ctx, "agentsvc: recording a failed card effect", "decisionId", r.ID, "error", err)
	}
}

// runEffect runs exactly the bound payload, after the answer stands. The target is checked again here:
// core by the catalogue's own checks and the writer's trust, the provisioner by the branch's head.
func (s *Server) runEffect(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect) error {
	if err := s.checkEffect(ctx, r, e); err != nil {
		return err
	}
	var err error
	switch e.GetVerb() {
	case decisions.VerbCatalogue:
		if s.cardFx.Remediate == nil {
			return refuse(errors.New("catalogue: no remediation service"))
		}
		_, err = s.cardFx.Remediate(ctx, cardOf(r).GetWriterDefinition(), []byte(e.GetPayload()), s.boundsOf(ctx, r))
	case decisions.VerbOpenPR:
		if s.cardFx.OpenPR == nil {
			return refuse(errors.New("open_pr: no provisioner"))
		}
		var t decisions.PRTarget
		if err := json.Unmarshal([]byte(e.GetPayload()), &t); err != nil {
			return err
		}
		_, err = s.cardFx.OpenPR(ctx, &agentfleetv1.OpenPullRequestRequest{
			Repo: t.Repo, Branch: t.Branch, HeadSha: t.HeadSHA, Title: t.Title,
			Body: "Opened by the owner's answer to decision card " + r.ID + ".",
		})
	case decisions.VerbOpenSession:
		if s.opener == nil {
			return refuse(errors.New("open_session: no console"))
		}
		var b decisions.Brief
		if err := json.Unmarshal([]byte(e.GetPayload()), &b); err != nil {
			return err
		}
		_, err = s.opener.OpenSupervised(ctx, b.Repo, b.Title, briefBody(r.ID, b))
	}
	if err != nil && connect.CodeOf(err) == connect.CodeUnknown {
		err = refuse(err)
	}
	return err
}

// briefBody is the seed entry: typed fields as they are, the title labelled an agent claim.
func briefBody(cardID string, b decisions.Brief) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[decision card %s, answered by the owner; typed fields, the title is an agent claim]\nrepo: %s\ntitle (agent claim): %s\n", cardID, b.Repo, b.Title)
	for _, f := range b.Findings {
		fmt.Fprintf(&sb, "%s: %g\n", f.Name, f.Value)
	}
	return sb.String()
}
