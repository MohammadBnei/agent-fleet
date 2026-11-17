package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// character is a started standing character's row and its pinned spec: who a spawn is for (docs/adr/0084 §6).
func (s *Server) character(ctx context.Context, id string) (*sessions.Session, *definitions.Spec, error) {
	t, err := s.sessions.Get(ctx, id)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		return nil, nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("no such session"))
	case err != nil:
		return nil, nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	case deref(t.Lifecycle, "") != sessions.Standing || t.ArchivedAt != nil || t.StoppedAt != nil || t.DefinitionName == nil || t.Invoker == nil:
		return nil, nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("only a started standing character may do this"))
	}
	raw, _, err := s.sessions.PinnedEffective(ctx, id)
	if err != nil {
		return nil, nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
	}
	var spec definitions.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return t, &spec, nil
}

// SpawnWorker starts one single-shot run of a definition on the character's spawns.allow (docs/adr/0084 §6): an
// in-process Invoke whose invoker is the character's own principal, T1, counted in the definition pool, MaxLive and
// the character's sub-pool (sessions.ReserveSlot), and capped per hour and per day here, on a NEW run only. Depth is
// 1: a spawned run is a single-shot definition, and none holds a reach tool (the loader refuses it).
func (s *Server) SpawnWorker(ctx context.Context, characterID, definition, input, key string) (string, error) {
	if err := checkInvokeBounds(key, input); err != nil {
		return "", err
	}
	t, spec, err := s.character(ctx, characterID)
	if err != nil {
		return "", err
	}
	if !spec.MaySpawn(definition) {
		return "", errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, fmt.Errorf("%s is not on this character's spawns.allow", definition))
	}
	head, err := s.definitions.Resolve(definition)
	if err != nil || head.Effective == nil || head.SHA == "" {
		return "", errkeys.Connect(connect.CodeNotFound, errkeys.KeyDefinitionUnavailable, definitions.ErrUnknown)
	}
	invoker := *t.Invoker
	_, perHour, perDay := spec.SpawnCaps()
	admit := func(ctx context.Context) error {
		for _, w := range []struct {
			d time.Duration
			n int
			s string
		}{{time.Hour, perHour, "hour"}, {24 * time.Hour, perDay, "day"}} {
			n, err := s.sessions.SpawnsSince(ctx, invoker, w.d)
			if err != nil {
				return errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
			}
			if n >= w.n {
				return errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, fmt.Errorf("this character started %d runs in the last %s (limits.spawns allows %d)", n, w.s, w.n))
			}
		}
		return nil
	}
	// The caps count, then run creates the thread: one lock per character keeps parallel spawns from all passing
	// the count. ponytail: in-process, core is a single replica; an advisory lock if it ever replicates.
	mu, _ := s.spawnLocks.LoadOrStore(invoker, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	p := Principal{Client: invoker}
	res, err := s.run(ctx, p, &agentfleetv1.InvokeRequest{Definition: definition, Input: input, IdempotencyKey: key}, head, sessions.TierFleet, admit)
	if err != nil {
		return "", err
	}
	return res.Msg.GetThreadId(), nil
}

// SpawnResult is WaitForResult for a run this character started and nobody else's (docs/adr/0084 §6).
func (s *Server) SpawnResult(ctx context.Context, characterID, threadID string, wait time.Duration) (*agentfleetv1.WaitForResultResponse, error) {
	t, _, err := s.character(ctx, characterID)
	if err != nil {
		return nil, err
	}
	if wait < 0 || wait > maxWait {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("wait_seconds must be 0 to %d", int(maxWait/time.Second)))
	}
	run, _, err := s.lookup(ctx, Principal{Client: *t.Invoker}, threadID, true)
	if err != nil {
		return nil, err
	}
	// lookup limits the owner to this character's (owner is client:<invoker>); a single-shot run excludes its own row.
	if deref(run.Lifecycle, "") != singleShot {
		return nil, errNoThread
	}
	res, err := s.awaitResult(ctx, run, wait)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// maxSpawnEntry bounds the text of a run's end entry: the result is the run's own, and the entry is read on the
// character's next turn.
const maxSpawnEntry = 8 << 10

// ReportSpawnEnds tells each started character that a run it spawned has ended, once, by appending a turn-starting
// entry to its main thread (docs/adr/0084 §5, §6). The run's text is T1: a spawned run's set lacks A. A character
// that is not started takes nothing and the run is marked reported anyway: it asks with WaitForResult if it comes
// back. A pass that fails on one run leaves it for the next.
func (s *Server) ReportSpawnEnds(ctx context.Context) error {
	ends, err := s.sessions.SpawnEnds(ctx, 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ends {
		if err := s.reportSpawnEnd(ctx, e); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", e.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) reportSpawnEnd(ctx context.Context, e sessions.SpawnEnd) error {
	run, err := s.sessions.Get(ctx, e.ID)
	if err != nil {
		return err
	}
	res, err := s.result(ctx, run)
	if err != nil {
		return err
	}
	if st := res.GetState(); st == "running" || st == "waiting" {
		return nil
	}
	char, err := s.sessions.StandingByDefinition(ctx, e.Character)
	if err != nil || char.StoppedAt != nil {
		if err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return err
		}
		return s.sessions.MarkSpawnReported(ctx, e.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your spawned run %s of %s ended: %s", e.ID, e.Run, res.GetState())
	if c := res.GetError().GetCode(); c != "" {
		fmt.Fprintf(&b, " (%s)", c)
	}
	if r := res.GetResultText(); r != "" {
		b.WriteString("\n\n" + r)
	}
	if o := res.GetStructuredOutput(); o != "" {
		b.WriteString("\n\nStructured output: " + o)
	}
	text := b.String()
	if len(text) > maxSpawnEntry {
		text = strings.ToValidUTF8(text[:maxSpawnEntry], "") + "\n[cut]"
	}
	// Warm BEFORE appending, and hold the release until the append lands (dashboard.WarmIfIdle).
	_, release, err := s.dash.WarmForAppend(ctx, char.ID)
	defer release()
	if err != nil {
		return fmt.Errorf("warm character: %w", err)
	}
	if _, err := s.transcr.Append(ctx, char.ID, "session", text, "discussion", "spawn-end:"+e.ID); err != nil {
		return err
	}
	slog.Info("agentsvc: told a character its spawned run ended", "character", e.Character, "run", e.ID, "state", res.GetState())
	return s.sessions.MarkSpawnReported(ctx, e.ID)
}

// RunSpawnReports calls ReportSpawnEnds every interval until ctx ends.
func (s *Server) RunSpawnReports(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.ReportSpawnEnds(ctx); err != nil {
				slog.Error("agentsvc: reporting spawned runs", "error", err)
			}
		}
	}
}
