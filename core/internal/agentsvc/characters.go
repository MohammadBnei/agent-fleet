package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The character API for the game (docs/adr/0084 amendment A1-A3): the owner reads every character, started or not.

// characterPoll is how often WatchCharacters looks again. ponytail: a poll of every character's row, as WatchDecisions
// polls its table; a handful of characters make it cheap, an event feed if they ever number in the hundreds.
var characterPoll = 2 * time.Second

var errOwnerOnly = errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only the owner reads characters"))

// ownerOnly is A1's second half: the table lets a member in, and the handler refuses anyone but an admin on the
// console or an admin origin. It is not Admin(), which is false on a surface.
func ownerOnly(ctx context.Context) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if !p.AnswersCards() {
		return errOwnerOnly
	}
	return nil
}

// ListCharacters is every character with its state now.
func (s *Server) ListCharacters(ctx context.Context, _ *connect.Request[agentfleetv1.ListCharactersRequest]) (*connect.Response[agentfleetv1.ListCharactersResponse], error) {
	if err := ownerOnly(ctx); err != nil {
		return nil, err
	}
	cs, err := s.characters(ctx)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.ListCharactersResponse{Characters: cs}), nil
}

// WatchCharacters sends every character, then each one that changed, until the stream's age is up.
func (s *Server) WatchCharacters(ctx context.Context, _ *connect.Request[agentfleetv1.WatchCharactersRequest], stream *connect.ServerStream[agentfleetv1.WatchCharactersResponse]) error {
	if err := ownerOnly(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, streamMaxAge)
	defer cancel()
	sent := map[string]*agentfleetv1.Character{}
	tick := time.NewTicker(characterPoll)
	defer tick.Stop()
	for first := true; ; first = false {
		cs, err := s.characters(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		var changed []*agentfleetv1.Character
		for _, c := range cs {
			if old, ok := sent[c.GetName()]; !ok || !proto.Equal(old, c) {
				changed = append(changed, c)
				sent[c.GetName()] = c
			}
		}
		removed := dropGone(sent, cs)
		// The baseline always goes out, empty or not; a quiet poll sends nothing.
		if first || len(changed) > 0 || len(removed) > 0 {
			if err := stream.Send(&agentfleetv1.WatchCharactersResponse{Characters: changed, Removed: removed}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// characters is every valid standing definition's character plus every started one whose head is invalid or gone.
// A never-started character with a broken head cannot be started, so it is not listed.
func (s *Server) characters(ctx context.Context) ([]*agentfleetv1.Character, error) {
	if s.definitions == nil {
		return nil, nil
	}
	heads, _ := s.definitions.List()
	cards := map[string]int{}
	if s.decisions != nil {
		var err error
		if cards, err = s.decisions.OpenCardsByCharacter(ctx, sessions.StandingPrefix); err != nil {
			return nil, err
		}
	}
	out := []*agentfleetv1.Character{}
	listed := map[string]bool{}
	for _, h := range heads {
		if h.Effective == nil || h.Effective.Lifecycle == nil || !slices.Contains(h.Effective.Lifecycle.Allow, sessions.Standing) {
			continue
		}
		c, err := s.characterView(ctx, h, cards[h.Name])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		listed[h.Name] = true
	}
	// A started character whose head is invalid or gone still runs its pinned version: the owner keeps seeing it.
	var names []string
	if s.sessions != nil {
		var err error
		if names, err = s.sessions.LiveStandingNames(ctx); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		if listed[n] {
			continue
		}
		c, err := s.characterView(ctx, definitions.Head{Name: n}, cards[n])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		listed[n] = true
	}
	return out, nil
}

func (s *Server) characterView(ctx context.Context, h definitions.Head, openCards int) (*agentfleetv1.Character, error) {
	spec, sha := h.Effective, h.SHA
	if spec == nil {
		spec = &definitions.Spec{}
	}
	row, err := s.sessions.StandingByDefinition(ctx, h.Name)
	if errors.Is(err, sessions.ErrNotFound) {
		return characterProto(h.Name, sha, spec, nil, sessions.CharacterRuntime{}, openCards), nil
	}
	if err != nil {
		return nil, err
	}
	rt, err := s.sessions.CharacterRuntime(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	// The version the character runs, not the head: a persona change shows once the drain has re-pinned it.
	if raw, _, err := s.sessions.PinnedEffective(ctx, row.ID); err == nil && raw != nil {
		var pinned definitions.Spec
		if json.Unmarshal(raw, &pinned) == nil {
			spec = &pinned
		}
	}
	return characterProto(h.Name, rt.SHA, spec, row, rt, openCards), nil
}

// characterProto is the pure half: a character's message from its pinned spec and what its row says.
func characterProto(name, sha string, spec *definitions.Spec, row *sessions.Session, rt sessions.CharacterRuntime, openCards int) *agentfleetv1.Character {
	budget := spec.TurnBudget()
	held := row != nil && rt.TurnsToday >= budget && !rt.Overridden
	state, why := characterState(row != nil, rt, held)
	c := &agentfleetv1.Character{
		Name: name, DisplayName: name, Definition: &agentfleetv1.CharacterDefinition{Name: name, Sha: sha},
		State: state, Why: why, CanSpeak: slices.Contains(spec.SidecarTools, "speak"), OpenCards: int32(openCards),
		Budget: &agentfleetv1.CharacterBudget{TurnsToday: int32(rt.TurnsToday), Limit: int32(budget), Held: held},
		Spawns: &agentfleetv1.CharacterSpawns{Used: int32(rt.SpawnsLive), Limit: int32(spec.SpawnSubPool())},
	}
	if p := spec.Persona; p != nil {
		c.DisplayName = p.DisplayName
		c.Persona = &agentfleetv1.CharacterPersona{DisplayName: p.DisplayName, Role: p.Role, Traits: slices.Clone(p.Traits), Portrait: p.Portrait, Home: p.Home}
		for _, r := range p.Relations {
			c.Persona.Relations = append(c.Persona.Relations, &agentfleetv1.CharacterRelation{To: r.To, Kind: r.Kind})
		}
	}
	for _, t := range rt.Threads {
		c.Threads = append(c.Threads, &agentfleetv1.CharacterThread{Key: t.Key, Title: t.Title, Running: t.Running, LastAt: t.LastAt.UTC().Format(time.RFC3339)})
	}
	if rt.RunningThread != "" {
		c.RunningTurn = &agentfleetv1.CharacterTurn{Thread: rt.RunningThread, StartedAt: rt.RunningSince.UTC().Format(time.RFC3339)}
	}
	return c
}

// characterState is amendment A3's state and why. stopping is a stopped character whose pod is still going down;
// running keeps the drain's reason beside it, since the drain waits for the turn.
func characterState(started bool, rt sessions.CharacterRuntime, budgetHeld bool) (state, why string) {
	drain := ""
	if rt.Draining {
		drain = "pin drain"
		if rt.Aged {
			drain = "token refresh"
		}
	}
	switch {
	case !started || (rt.Stopped && !rt.PodLive):
		return "stopped", ""
	case rt.Stopped:
		return "stopping", "pod drain"
	case rt.RunningThread != "":
		return "running", drain
	case rt.Draining:
		return "held", drain
	case !rt.PodLive && rt.Crashes > 0:
		return "held", "crash back-off"
	case budgetHeld:
		return "held", "budget"
	case !rt.PodRunning:
		return "starting", ""
	}
	return "idle", ""
}

// dropGone forgets and names every sent character that is not in cs any more, in order.
func dropGone(sent map[string]*agentfleetv1.Character, cs []*agentfleetv1.Character) []string {
	now := map[string]bool{}
	for _, c := range cs {
		now[c.GetName()] = true
	}
	var gone []string
	for n := range sent {
		if !now[n] {
			gone = append(gone, n)
			delete(sent, n)
		}
	}
	slices.Sort(gone)
	return gone
}
