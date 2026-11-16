package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/MohammadBnei/ukubi-go/errs"

	"encoding/json"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// CardEffects is what the core-run card verbs reach (docs/adr/0086 §4). A nil field refuses its verb.
type CardEffects struct {
	// Remediate is remediations.Service.Select: the schema, denylist and rate limits, then a pending row.
	Remediate func(ctx context.Context, responder string, raw []byte, b remediations.Bounds) (string, error)
	// OpenPR is the provisioner's OpenPullRequest.
	OpenPR func(ctx context.Context, req *agentfleetv1.OpenPullRequestRequest) (*agentfleetv1.OpenPullRequestResponse, error)
}

// SetCardEffects wires the catalogue and open_pr verbs.
func (s *Server) SetCardEffects(fx CardEffects) { s.cardFx = fx }

// sessionOpener starts open_session's session (*dashboard.Server).
type sessionOpener interface {
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

func refuse(key errs.Key, err error) error {
	return errkeys.Connect(connect.CodeFailedPrecondition, key, err)
}

// checkEffect runs before the answer is recorded: a refusal leaves the card open. The approval binds to
// the payload hash, and open_pr's writer must still be a B.C session.
func (s *Server) checkEffect(ctx context.Context, r decisions.Record, e *agentfleetv1.CardEffect) error {
	if e.GetVerb() == decisions.VerbExecutorStep {
		return refuse(errkeys.KeyInternal, errors.New("executor_step is declared but refused until ADR-0072's executor exists"))
	}
	if err := decisions.VerifyEffect(e); err != nil {
		return refuse(errkeys.KeyDecisionStale, err)
	}
	if e.GetVerb() == decisions.VerbOpenPR {
		return s.writerIsBC(ctx, r)
	}
	return nil
}

func (s *Server) writerIsBC(ctx context.Context, r decisions.Record) error {
	w, err := s.sessions.Get(ctx, r.SessionID)
	if err != nil || w == nil {
		return refuse(errkeys.KeyInternal, fmt.Errorf("open_pr: the writer %s is gone", r.SessionID))
	}
	if w.TrustSet != "BC" {
		return refuse(errkeys.KeyInternal, fmt.Errorf("open_pr needs a B.C writer, this one holds %q", w.TrustSet))
	}
	return nil
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
			return refuse(errkeys.KeyInternal, errors.New("catalogue: no remediation service"))
		}
		var b remediations.Bounds
		if raw, _, perr := s.sessions.PinnedEffective(ctx, r.SessionID); perr == nil {
			var eff definitions.Spec
			if json.Unmarshal(raw, &eff) == nil && eff.Trust != nil && eff.Trust.Scale != nil {
				b = remediations.Bounds{Min: eff.Trust.Scale.Min, Max: eff.Trust.Scale.Max}
			}
		}
		_, err = s.cardFx.Remediate(ctx, cardOf(r).GetWriterDefinition(), []byte(e.GetPayload()), b)
	case decisions.VerbOpenPR:
		if s.cardFx.OpenPR == nil {
			return refuse(errkeys.KeyInternal, errors.New("open_pr: no provisioner"))
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
			return refuse(errkeys.KeyInternal, errors.New("open_session: no console"))
		}
		var b decisions.Brief
		if err := json.Unmarshal([]byte(e.GetPayload()), &b); err != nil {
			return err
		}
		_, err = s.opener.OpenSupervised(ctx, b.Repo, b.Title, briefBody(r.ID, b))
	}
	if err != nil && connect.CodeOf(err) == connect.CodeUnknown {
		err = refuse(errkeys.KeyInternal, err)
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
