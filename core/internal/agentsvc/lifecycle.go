package agentsvc

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The character API's lifecycle and thread reads (docs/adr/0084 amendment A4, A5), owner only. Start, Stop and Close
// are the console's DashboardService paths (dashboard/standing.go), never a second one: this layer only checks the
// owner, turns the outcomes that are states into states, and answers with the character as ListCharacters shows it.

// threadPoll is how often WatchCharacterThread reads the transcript again. ponytail: a poll of one owner's one stream
// each second; the console's hub wake if streams ever number in the dozens.
var threadPoll = time.Second

const threadPage = 500

// characterNamed is the named character as ListCharacters shows it now: agent.character_not_found when no standing
// definition and no started character has the name.
func (s *Server) characterNamed(ctx context.Context, name string) (*agentfleetv1.Character, error) {
	cs, err := s.characters(ctx)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	for _, c := range cs {
		if c.GetName() == name {
			return c, nil
		}
	}
	return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyCharacterNotFound, errors.New("no such character"))
}

// lifecycle is Start's and Stop's shared shape: the owner, the character, the console's call, and its outcomes that
// only say the state already is what was asked.
func (s *Server) lifecycle(ctx context.Context, name string, call func() error, already ...error) (*agentfleetv1.Character, error) {
	if err := ownerOnly(ctx); err != nil {
		return nil, err
	}
	if _, err := s.characterNamed(ctx, name); err != nil {
		return nil, err
	}
	if err := call(); err != nil {
		if errors.Is(err, dashboard.ErrStandingUnknown) {
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyCharacterNotFound, err)
		}
		if !anyIs(err, already) {
			return nil, err
		}
	}
	return s.characterNamed(ctx, name)
}

func anyIs(err error, targets []error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

// StartCharacter makes the character's session or resumes a stopped one, and answers with its state at once. A started
// one answers its state; one whose old pod is still going down answers "stopping" (why "pod drain") for the game to
// show and retry.
func (s *Server) StartCharacter(ctx context.Context, req *connect.Request[agentfleetv1.StartCharacterRequest]) (*connect.Response[agentfleetv1.StartCharacterResponse], error) {
	c, err := s.lifecycle(ctx, req.Msg.GetName(), func() error {
		_, err := s.dash.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: req.Msg.GetName()}))
		return err
	}, dashboard.ErrStandingStarted, dashboard.ErrStandingStopping)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.StartCharacterResponse{Character: c}), nil
}

// StopCharacter stops the character and answers with its state at once. Every thread keeps its SDK session id and
// cursor, and only a turn in flight is aborted. A stopped or never-started character answers its state.
func (s *Server) StopCharacter(ctx context.Context, req *connect.Request[agentfleetv1.StopCharacterRequest]) (*connect.Response[agentfleetv1.StopCharacterResponse], error) {
	c, err := s.lifecycle(ctx, req.Msg.GetName(), func() error {
		_, err := s.dash.StopStanding(ctx, connect.NewRequest(&agentfleetv1.StopStandingRequest{Definition: req.Msg.GetName()}))
		return err
	}, dashboard.ErrStandingNever, dashboard.ErrStandingStopped)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.StopCharacterResponse{Character: c}), nil
}

// CloseCharacterThread parks one thread: its SDK session id and transcript stay, and the owner's next message to the
// key resumes it. A parked thread answers success; main and an unknown key are agent.thread_not_found.
func (s *Server) CloseCharacterThread(ctx context.Context, req *connect.Request[agentfleetv1.CloseCharacterThreadRequest]) (*connect.Response[agentfleetv1.CloseCharacterThreadResponse], error) {
	if err := ownerOnly(ctx); err != nil {
		return nil, err
	}
	name, key := req.Msg.GetName(), req.Msg.GetThread()
	if _, err := s.characterNamed(ctx, name); err != nil {
		return nil, err
	}
	_, err := s.dash.CloseStandingThread(ctx, connect.NewRequest(&agentfleetv1.CloseStandingThreadRequest{Definition: name, ThreadKey: key}))
	switch {
	case err == nil, errors.Is(err, dashboard.ErrThreadParked):
	case errors.Is(err, dashboard.ErrNoThread), errors.Is(err, dashboard.ErrStandingNever):
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyThreadNotFound, err)
	default:
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.CloseCharacterThreadResponse{}), nil
}

// threadSession is the character's session when it has the thread, closed or parked included.
func (s *Server) threadSession(ctx context.Context, name, key string) (string, error) {
	if _, err := s.characterNamed(ctx, name); err != nil {
		return "", err
	}
	row, err := s.sessions.StandingByDefinition(ctx, name)
	if errors.Is(err, sessions.ErrNotFound) {
		return "", errkeys.Connect(connect.CodeNotFound, errkeys.KeyThreadNotFound, errors.New("the character was never started"))
	}
	if err != nil {
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	threads, err := s.sessions.Threads(ctx, row.ID)
	if err != nil {
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	for _, t := range threads {
		if t.Key == key {
			return row.ID, nil
		}
	}
	return "", errkeys.Connect(connect.CodeNotFound, errkeys.KeyThreadNotFound, errors.New("no such thread"))
}

// readThread is the thread's entries from since, at most limit, and where to read on from. The transcript is one
// sequence for every thread, so a page is filtered, not cut.
func (s *Server) readThread(ctx context.Context, p Principal, id, key string, since int64, limit int) ([]*agentfleetv1.Entry, int64, error) {
	var out []*agentfleetv1.Entry
	cur := since
	for {
		entries, next, err := s.transcr.ReadSince(ctx, id, cur, threadPage)
		if err != nil {
			return nil, 0, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
		for _, e := range entries {
			if e.ThreadKey != key {
				continue
			}
			if len(out) == limit {
				return out, e.Seq, nil // the next entry starts the next page
			}
			out = append(out, entryToProto(p, e))
		}
		cur = next
		if len(entries) < threadPage {
			return out, cur, nil
		}
	}
}

// ReadCharacterThread is one thread's transcript in full, the entries the console shows, tool results included
// (amendment A5: owner-readable, though the standing row is owned by client:standing:<name>).
func (s *Server) ReadCharacterThread(ctx context.Context, req *connect.Request[agentfleetv1.ReadCharacterThreadRequest]) (*connect.Response[agentfleetv1.ReadCharacterThreadResponse], error) {
	if err := ownerOnly(ctx); err != nil {
		return nil, err
	}
	p, _ := FromContext(ctx)
	id, err := s.threadSession(ctx, req.Msg.GetName(), req.Msg.GetThread())
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > threadPage {
		limit = threadPage
	}
	entries, next, err := s.readThread(ctx, p, id, req.Msg.GetThread(), req.Msg.GetSince(), limit)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.ReadCharacterThreadResponse{Entries: entries, NextSeq: next}), nil
}

// WatchCharacterThread sends the thread from since, then each new entry, until the stream's age is up. The first frame
// always goes out, empty or not, so the client knows it is connected.
func (s *Server) WatchCharacterThread(ctx context.Context, req *connect.Request[agentfleetv1.WatchCharacterThreadRequest], stream *connect.ServerStream[agentfleetv1.WatchCharacterThreadResponse]) error {
	if err := ownerOnly(ctx); err != nil {
		return err
	}
	p, _ := FromContext(ctx)
	key := req.Msg.GetThread()
	id, err := s.threadSession(ctx, req.Msg.GetName(), key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, streamMaxAge)
	defer cancel()
	cur := req.Msg.GetSince()
	tick := time.NewTicker(threadPoll)
	defer tick.Stop()
	for first := true; ; first = false {
		entries, next, err := s.readThread(ctx, p, id, key, cur, 1<<30)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if first || len(entries) > 0 {
			if err := stream.Send(&agentfleetv1.WatchCharacterThreadResponse{Entries: entries, NextSeq: next}); err != nil {
				return err
			}
		}
		cur = next
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
