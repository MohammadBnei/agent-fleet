//go:build integration

package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0084 §2: Start makes the character's one row under its own principal, Stop keeps it down, Start resumes
// it with a fresh SDK session, and no other path opens it.
func TestStandingStartStop(t *testing.T) {
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}})
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-stander.yaml"] = []byte(strings.NewReplacer("name: dev-dream-analyst }", "name: dev-stander }",
			"vault: dev-dream-analyst", "vault: dev-stander").Replace(string(p.files["definitions/dev-dream-analyst.yaml"])) +
			"  lifecycle: { allow: [standing] }\n")
	})
	if err := reg.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := func(ctx context.Context, def string) (*agentfleetv1.Session, error) {
		res, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: def}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetSession(), nil
	}
	stop := func(def string) error {
		_, err := s.StopStanding(ctx, connect.NewRequest(&agentfleetv1.StopStandingRequest{Definition: def}))
		return err
	}

	// The wrong callers and the wrong definitions.
	if _, err := start(context.Background(), "dev-stander"); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("Start with no identity: %v", err)
	}
	if _, err := start(ctx, "dev-dream-analyst"); code(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrStandingUnknown) {
		t.Fatalf("Start of a definition that is not standing: %v", err)
	}
	if _, err := start(ctx, "nope"); code(err) != connect.CodeNotFound || !errors.Is(err, ErrStandingUnknown) {
		t.Fatalf("Start of an unknown definition: %v", err)
	}
	if err := stop("dev-stander"); code(err) != connect.CodeNotFound || !errors.Is(err, ErrStandingNever) {
		t.Fatalf("Stop of a character never started: %v", err)
	}
	// The console's CreateSession is no way in either.
	if _, err := createDef(s, "dev-stander", nil); errkeys.Of(err) != errkeys.KeyStandingOnly {
		t.Fatalf("CreateSession of a standing definition: %v", err)
	}

	row, err := start(ctx, "dev-stander")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, row.GetId())
	if err != nil {
		t.Fatal(err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if deref(got.Lifecycle) != "standing" || deref(got.Invoker) != "standing:dev-stander" || deref(got.Owner) != "client:standing:dev-stander" ||
		deref(got.StartedBy) != "root" || got.TrustSet != "BC" || got.StoppedAt != nil || deref(got.DefinitionSHA) != "sha-one" {
		t.Fatalf("the row: lifecycle %q invoker %q owner %q startedBy %q set %q stopped %v sha %q",
			deref(got.Lifecycle), deref(got.Invoker), deref(got.Owner), deref(got.StartedBy), got.TrustSet, got.StoppedAt, deref(got.DefinitionSHA))
	}
	if _, err := start(ctx, "dev-stander"); code(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrStandingStarted) {
		t.Fatalf("Start of a running character: %v", err)
	}

	// Stop: marked, an abort entry for a live pod, and no warm until Start.
	if err := stop("dev-stander"); err != nil {
		t.Fatal(err)
	}
	if err := stop("dev-stander"); code(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrStandingStopped) {
		t.Fatalf("Stop of a stopped character: %v", err)
	}
	entries, _, err := s.transcr.ReadSince(ctx, row.GetId(), 0, 100)
	if err != nil || len(entries) != 1 || entries[0].Type != "abort" {
		t.Fatalf("Stop's transcript: %v %v", entries, err)
	}
	if got, _ = store.Get(ctx, row.GetId()); got.StoppedAt == nil {
		t.Fatal("Stop did not mark the row")
	}
	if _, err := s.WarmSession(ctx, connect.NewRequest(&agentfleetv1.WarmSessionRequest{SessionId: row.GetId()})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(errText(err), "stopped") || prov.createCount() != 0 {
		t.Fatalf("a stopped character warmed: %v (%d pods)", err, prov.createCount())
	}

	// A pod still live inside the stop-grace window: Start is refused and the teardown mark stays armed.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, row.GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := start(ctx, "dev-stander"); code(err) != connect.CodeAborted || !errors.Is(err, ErrStandingStopping) {
		t.Fatalf("Start over a live pod: %v", err)
	}
	var armed bool
	if err := pool.QueryRow(ctx, `SELECT stopped_at IS NOT NULL AND stop_requested_at IS NOT NULL FROM sessions WHERE id = $1`, row.GetId()).Scan(&armed); err != nil || !armed {
		t.Fatalf("a refused Start disarmed the stop: %v %v", armed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = NULL WHERE id = $1`, row.GetId()); err != nil {
		t.Fatal(err)
	}

	// Start resumes the same row with its SDK session (amendment A4), and the transcript stays.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET agent_session_id = 'old-sdk', definition_sha = definition_sha WHERE id = $1`, row.GetId()); err != nil {
		t.Fatal(err)
	}
	again, err := start(ctx, "dev-stander")
	if err != nil || again.GetId() != row.GetId() {
		t.Fatalf("Start of a stopped character: %v %v", again.GetId(), err)
	}
	if got, _ = store.Get(ctx, row.GetId()); got.StoppedAt != nil || got.AgentSessionID == nil || *got.AgentSessionID != "old-sdk" {
		t.Fatalf("resumed row: stopped %v sdk session %v, want it kept", got.StoppedAt, got.AgentSessionID)
	}
	if entries, _, _ = s.transcr.ReadSince(ctx, row.GetId(), 0, 100); len(entries) != 1 {
		t.Fatalf("the transcript did not stay: %v", entries)
	}
}

// docs/adr/0084 §2: the schema holds the shape, so no writer can leave it half made.
func TestStandingSchemaChecks(t *testing.T) {
	_, _, _, _, pool := newDefinitionsServerPool(t)
	ctx := context.Background()
	const ins = `INSERT INTO sessions (repo, definition_name, definition_sha, owner, invoker, thread_key, lifecycle, started_by, stopped_at)
		VALUES ('dream-analyst', 'dev-dream-analyst', 'sha-one', $1, $2, $3, $4, $5, $6)`
	for _, tc := range []struct {
		name string
		args []any
		ok   bool
	}{
		{"a standing row", []any{"client:standing:dev-dream-analyst", "standing:dev-dream-analyst", "main", "standing", "root", nil}, true},
		{"standing under a client's principal", []any{"client:bridge", "bridge", "main", "standing", "root", nil}, false},
		{"standing on another thread key", []any{"client:standing:dev-dream-analyst", "standing:dev-dream-analyst", "other", "standing", "root", nil}, false},
		{"standing with no admin", []any{"client:standing:dev-dream-analyst", "standing:dev-dream-analyst", "main", "standing", nil, nil}, false},
		{"started_by on a resident row", []any{"client:bridge", "bridge", "k", "resident", "root", nil}, false},
		{"stopped_at on a resident row", []any{"client:bridge", "bridge", "k", "resident", nil, "now()"}, false},
	} {
		if tc.args[5] == "now()" {
			tc.args[5] = timeNow()
		}
		_, err := pool.Exec(ctx, ins, tc.args...)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func timeNow() time.Time { return time.Now() }

// ADR-0086 §5: a loaded standing definition reaches the pod profile as Standing (memory, Guaranteed QoS) with the
// row's stored set; the same definition's non-standing session would not.
func TestStandingProfileCarriesMemoryGate(t *testing.T) {
	s, prov, reg, _, pool := newDefinitionsServerPool(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}})
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-stander.yaml"] = []byte(strings.NewReplacer("name: dev-dream-analyst }", "name: dev-stander }",
			"vault: dev-dream-analyst", "vault: dev-stander").Replace(string(p.files["definitions/dev-dream-analyst.yaml"])) +
			"  lifecycle: { allow: [standing] }\n  cluster: { read: true }\n")
	})
	if err := reg.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.caps.MaxStanding = 2
	res, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: "dev-stander"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WarmSession(ctx, connect.NewRequest(&agentfleetv1.WarmSessionRequest{SessionId: res.Msg.GetSession().GetId()})); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	d := prov.creates[len(prov.creates)-1].GetDefinition()
	prov.mu.Unlock()
	if !d.GetStanding() || d.GetTrustSet() != "BC" {
		t.Errorf("profile standing %v set %q, want standing with the stored set BC", d.GetStanding(), d.GetTrustSet())
	}
	// ADR-0084 §6: cluster.read reaches the profile.
	if !d.GetClusterRead() {
		t.Error("cluster.read did not reach the profile")
	}
	// ADR-0086 §2: cluster reads are B; a set without it (A·C) gets no service account.
	id := res.Msg.GetSession().GetId()
	// The narrowing trigger refuses BC -> AC, so stage the row with it off (throwaway database).
	if _, err := pool.Exec(ctx, `ALTER TABLE sessions DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET trust_set = 'AC', pod_phase = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WarmSession(ctx, connect.NewRequest(&agentfleetv1.WarmSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	d = prov.creates[len(prov.creates)-1].GetDefinition()
	prov.mu.Unlock()
	if d.GetClusterRead() {
		t.Error("a set without B got the cluster-read service account")
	}
}

// docs/adr/0084 §4: the owner's first message to a key opens a thread, a thread other than main is refused on any
// other row, a close drops the SDK session and tells a live pod, and the re-warm resumes from the thread's own
// cursor, never from LatestSeq.
func TestStandingThreads_OpenCloseAndResume(t *testing.T) {
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}})
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-stander.yaml"] = []byte(strings.NewReplacer("name: dev-dream-analyst }", "name: dev-stander }",
			"vault: dev-dream-analyst", "vault: dev-stander").Replace(string(p.files["definitions/dev-dream-analyst.yaml"])) +
			"  lifecycle: { allow: [standing] }\n")
	})
	if err := reg.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: "dev-stander"}))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.GetSession().GetId()
	s.caps = sessions.Caps{MaxLive: 5, MaxDefinition: 2, MaxExternal: 2, MaxPerMember: 2, MaxStanding: 2}
	post := func(thread, text string) error {
		_, err := s.PostMessage(ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: id, Text: text, ThreadKey: thread}))
		return err
	}
	threads := func() map[string]sessions.Thread {
		t.Helper()
		th, err := store.Threads(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]sessions.Thread{}
		for _, x := range th {
			m[x.Key] = x
		}
		return m
	}

	if err := post("Bad Key", "x"); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a malformed thread key: %v", err)
	}
	if err := post("topic-a", "hello"); err != nil {
		t.Fatal(err)
	}
	if prov.createCount() != 1 {
		t.Fatalf("the first message did not warm the character: %d pods", prov.createCount())
	}
	if th := threads(); th["topic-a"].Closed || th["main"].Key != "main" || len(th) != 2 {
		t.Fatalf("threads after the first message = %+v", th)
	}
	entries, _, _ := s.transcr.ReadSince(ctx, id, 0, 100)
	if len(entries) != 1 || entries[0].ThreadKey != "topic-a" || entries[0].Text != "hello" {
		t.Fatalf("the entry's thread: %+v", entries)
	}

	// The pod goes down. A message arrives for a thread the pod never saw, and for the first one; the re-warm
	// starts at the lowest waiting entry, below LatestSeq.
	if err := store.SetPodPhase(ctx, id, agentfleetv1.PodPhase_POD_PHASE_CRASHED.String(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.transcr.Append(transcript.WithThread(ctx, "topic-b"), id, "human", "while down", "discussion", ""); err != nil {
		t.Fatal(err)
	}
	if err := post("topic-a", "second"); err != nil {
		t.Fatal(err)
	}
	if prov.createCount() != 2 {
		t.Fatalf("creates = %d, want the re-warm", prov.createCount())
	}
	prov.mu.Lock()
	next := prov.creates[len(prov.creates)-1]
	prov.mu.Unlock()
	if from := next.GetResumeFromSeq(); from != 0 {
		t.Fatalf("the re-warm streams from %d, want 0: topic-a's first message was never done", from)
	}
	// topic-a's result moves its cursor; the next re-warm starts at topic-b's entry, not at topic-a's.
	if _, err := s.transcr.Append(transcript.WithThread(ctx, "topic-a"), id, "agent", `{"subtype":"success","consumedSeq":2}`, "result", ""); err != nil {
		t.Fatal(err)
	}
	if from, ok, err := store.ResumeFrom(ctx, id); err != nil || !ok || from != 1 {
		t.Fatalf("ResumeFrom = %d %v %v, want 1 (topic-b's entry)", from, ok, err)
	}

	// Close: main never, an unknown key never, then the thread's SDK session id is dropped and an abort lands in it.
	closeT := func(key string) error {
		_, err := s.CloseStandingThread(ctx, connect.NewRequest(&agentfleetv1.CloseStandingThreadRequest{Definition: "dev-stander", ThreadKey: key}))
		return err
	}
	for _, key := range []string{"main", "nope"} {
		if err := closeT(key); code(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrNoThread) {
			t.Fatalf("close %q: %v", key, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = 'sdk-b' WHERE session_id = $1 AND thread_key = 'topic-b'`, id); err != nil {
		t.Fatal(err)
	}
	if err := closeT("topic-b"); err != nil {
		t.Fatal(err)
	}
	if err := closeT("topic-b"); code(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrThreadParked) {
		t.Fatalf("a thread closed twice: %v", err)
	}
	if th := threads()["topic-b"]; !th.Closed || th.AgentSessionID != "sdk-b" {
		t.Fatalf("topic-b after close = %+v, want parked with its SDK session", th)
	}
	var thr string
	if err := pool.QueryRow(ctx, `SELECT thread_key FROM transcript WHERE session_id = $1 AND type = 'abort' ORDER BY seq DESC LIMIT 1`, id).Scan(&thr); err != nil || thr != "topic-b" {
		t.Fatalf("the close's abort entry is in thread %q (%v)", thr, err)
	}
	if _, ok, _ := store.ResumeFrom(ctx, id); ok {
		t.Fatal("a closed thread's waiting entry still holds the resume point")
	}
	// An abort that cannot be written leaves the thread open: closed in the database under a live SDK session, the
	// owner could not send the abort again.
	real := s.transcr
	s.transcr = abortFails{real}
	if err := closeT("topic-a"); err == nil {
		t.Fatal("a close whose abort failed reported success")
	}
	if th := threads()["topic-a"]; th.Closed {
		t.Fatal("topic-a closed although its abort was never written")
	}
	s.transcr = real
	if err := closeT("topic-a"); err != nil {
		t.Fatalf("the retry of a close: %v", err)
	}
	// The key reopens, with its SDK session, on the next message.
	if err := post("topic-b", "back"); err != nil {
		t.Fatal(err)
	}
	if th := threads()["topic-b"]; th.Closed || th.AgentSessionID != "sdk-b" {
		t.Fatalf("topic-b did not reopen with its session: %+v", th)
	}

	// Any other row refuses a thread but main.
	plain, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: plain.GetId(), Text: "x", ThreadKey: "topic-a"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a thread on a resident session: %v", err)
	}
}

// abortFails is a transcript whose abort entries cannot be written.
type abortFails struct{ transcript.Store }

func (a abortFails) Append(ctx context.Context, id, from, text, typ, key string) (int64, error) {
	if typ == "abort" {
		return 0, errors.New("append refused")
	}
	return a.Store.Append(ctx, id, from, text, typ, key)
}

// docs/adr/0084 amendment A4: Stop keeps every thread's SDK session id, cursor and waiting entry, and aborts only a
// turn in flight (its head entry is settled, so Start does not replay it); Start resumes each thread where it was; a
// Stop's abort in main does not end the pod that resumes from below it; a thread closed and then written to resumes.
func TestStandingStopStart_KeepsThreadsAndAbortsOnlyTheTurn(t *testing.T) {
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Sub: "root", Groups: []string{auth.RequiredGroup}})
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-stander.yaml"] = []byte(strings.NewReplacer("name: dev-dream-analyst }", "name: dev-stander }",
			"vault: dev-dream-analyst", "vault: dev-stander").Replace(string(p.files["definitions/dev-dream-analyst.yaml"])) +
			"  lifecycle: { allow: [standing] }\n")
	})
	if err := reg.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: "dev-stander"}))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Msg.GetSession().GetId()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// main runs a turn on entry 1 (agent output above it, no result); chat has entry 3 waiting and a kept session.
	post := func(thread, from, typ, text string) int64 {
		t.Helper()
		seq, err := s.transcr.Append(transcript.WithThread(ctx, thread), id, from, text, typ, "")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	head := post("main", "human", "discussion", "work on the map")
	post("main", "agent", "assistant", "on it")
	waiting := post("chat", "human", "discussion", "when you are free")
	exec(`UPDATE sessions SET agent_session_id = 'sdk-main', pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, id)
	exec(`UPDATE session_threads SET agent_session_id = 'sdk-chat', cursor = $2 WHERE session_id = $1 AND thread_key = 'chat'`, id, waiting)
	cursor := func(key string) int64 {
		t.Helper()
		var c int64
		if err := pool.QueryRow(ctx, `SELECT cursor FROM session_threads WHERE session_id = $1 AND thread_key = $2`, id, key).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if running, _ := store.RunningHeads(ctx, id); running["main"] != head || len(running) != 1 {
		t.Fatalf("running turns %v, want main's at %d", running, head)
	}

	if _, err := s.StopStanding(ctx, connect.NewRequest(&agentfleetv1.StopStandingRequest{Definition: "dev-stander"})); err != nil {
		t.Fatal(err)
	}
	// Only the turn in flight is settled: main's cursor passes its head, chat's waiting entry stays above its cursor.
	if c := cursor("main"); c != head+1 {
		t.Errorf("main cursor %d, want %d: the aborted turn was not settled", c, head+1)
	}
	if c := cursor("chat"); c != waiting {
		t.Errorf("chat cursor %d, want %d: a waiting entry was consumed", c, waiting)
	}
	var settled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1 AND type = 'result' AND text LIKE '%aborted%'`, id).Scan(&settled); err != nil || settled != 1 {
		t.Errorf("%d aborted results (%v), want one, for the turn in flight", settled, err)
	}
	exec(`UPDATE sessions SET pod_phase = 'POD_PHASE_TERMINATED' WHERE id = $1`, id)
	if _, err := s.StartStanding(ctx, connect.NewRequest(&agentfleetv1.StartStandingRequest{Definition: "dev-stander"})); err != nil {
		t.Fatal(err)
	}
	// Start: every id and cursor as Stop left them, and the pod resumes from the waiting entry.
	if c := cursor("chat"); c != waiting {
		t.Errorf("chat cursor %d after start, want %d", c, waiting)
	}
	var chat string
	if err := pool.QueryRow(ctx, `SELECT agent_session_id FROM session_threads WHERE session_id = $1 AND thread_key = 'chat'`, id).Scan(&chat); err != nil || chat != "sdk-chat" {
		t.Errorf("chat's SDK session %q (%v), want sdk-chat", chat, err)
	}
	if got, _ := store.Get(ctx, id); got.AgentSessionID == nil || *got.AgentSessionID != "sdk-main" {
		t.Errorf("main's SDK session %v, want sdk-main", got.AgentSessionID)
	}
	if from, ok, err := store.ResumeFrom(ctx, id); err != nil || !ok || from != waiting {
		t.Errorf("the new pod resumes from %d (%v, %v), want %d", from, ok, err, waiting)
	}
}
