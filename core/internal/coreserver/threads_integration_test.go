//go:build integration

package coreserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

func newCharacter(t *testing.T) (*Server, *sessions.Store, *pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES
		('a', 'sha-a', '{}', '{"sidecarTools":["send_message","publish_event","draft_card"],"mode":{"default":"default","allow":["default"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, sessions.CreateParams{
		Repo: "r", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a",
		Owner: "client:standing:a", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:a", ThreadKey: "main", StartedBy: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{sessions: store, transcr: transcript.NewPostgresStore(pool, decisions.OnAppend)}
	return s, store, pool, id
}

func thread(t *testing.T, pool *pgxpool.Pool, id, key string, cursor int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO session_threads (session_id, thread_key, cursor) VALUES ($1, $2, $3)`, id, key, cursor); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, pool *pgxpool.Pool, id string, seq int64, th, from, typ, text string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
		VALUES ($1, $2, $3, $4, $5, gen_random_uuid()::text, $6)`, id, seq, from, text, typ, th); err != nil {
		t.Fatal(err)
	}
}

type streamRecorder struct {
	grpc.ServerStream
	ctx   context.Context
	texts []string
}

func (r *streamRecorder) Context() context.Context { return r.ctx }
func (r *streamRecorder) Send(e *agentfleetv1.TranscriptEntry) error {
	r.texts = append(r.texts, fmt.Sprintf("%s:%s", e.GetThreadKey(), e.GetText()))
	return nil
}

// docs/adr/0084 §4: a standing character is streamed per thread: nothing below its own thread's cursor, nothing
// of a thread the row does not have, and of a closed thread only the abort that ends its SDK session.
func TestStreamHumanMessages_StandingDeliversPerThread(t *testing.T) {
	s, _, pool, id := newCharacter(t)
	thread(t, pool, id, "a", 2)
	thread(t, pool, id, "b", 0)
	if _, err := pool.Exec(context.Background(), `UPDATE session_threads SET closed_at = now() WHERE session_id = $1 AND thread_key = 'b'`, id); err != nil {
		t.Fatal(err)
	}
	put(t, pool, id, 1, "a", "human", "discussion", "done")        // below a's cursor
	put(t, pool, id, 2, "a", "human", "discussion", "head")        // at it
	put(t, pool, id, 3, "b", "human", "discussion", "closed")      // b is closed
	put(t, pool, id, 4, "b", "human", "abort", "closed by root")   // ends b's SDK session
	put(t, pool, id, 5, "gone", "human", "discussion", "no row")   // a thread the row never had
	put(t, pool, id, 6, "main", "human", "discussion", "owner")    // main
	put(t, pool, id, 7, "a", "agent", "assistant", "agent output") // never streamed
	put(t, pool, id, 8, "a", "session", "discussion", "a peer")

	_, sha, err := s.agentSpecOf(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	rec := &streamRecorder{ctx: ctx}
	_ = s.StreamHumanMessages(&agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha}, rec)
	want := []string{"a:head", "b:closed by root", "main:owner", "a:a peer"}
	if !slices.Equal(rec.texts, want) {
		t.Fatalf("streamed %v, want %v", rec.texts, want)
	}
}

func TestGetSession_StandingCarriesThreadSessionIDs(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	thread(t, pool, id, "a", 0)
	thread(t, pool, id, "b", 0)
	thread(t, pool, id, "c", 0)
	if _, err := pool.Exec(context.Background(), `UPDATE session_threads SET agent_session_id = 'sdk-' || thread_key WHERE session_id = $1 AND thread_key IN ('a', 'b')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE session_threads SET closed_at = now() WHERE session_id = $1 AND thread_key = 'b'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE sessions SET agent_session_id = 'sdk-main' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_ = store
	res, err := s.GetSession(context.Background(), &agentfleetv1.GetSessionRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	// Open threads other than main that have one; main resumes from the row's own id.
	if got := res.GetThreadSessionIds(); len(got) != 1 || got["a"] != "sdk-a" {
		t.Fatalf("thread session ids = %v, want only a", got)
	}
	if res.GetAgentSpec().GetLifecycle() != "standing" {
		t.Fatalf("spec lifecycle = %q, want standing", res.GetAgentSpec().GetLifecycle())
	}
}

func TestSaveAgentSessionId_PerThread(t *testing.T) {
	s, _, pool, id := newCharacter(t)
	ctx := context.Background()
	thread(t, pool, id, "a", 0)
	var lease string
	if err := pool.QueryRow(ctx, `UPDATE sessions SET profile_lease_id = gen_random_uuid(), pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1 RETURNING profile_lease_id::text`, id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	save := func(sdk, th string) {
		t.Helper()
		if _, err := s.SaveAgentSessionId(ctx, &agentfleetv1.SaveAgentSessionIdRequest{SessionId: id, AgentSessionId: sdk, LeaseId: lease, ThreadKey: th}); err != nil {
			t.Fatal(err)
		}
	}
	save("sdk-main", "")
	save("sdk-main-2", "main")
	save("sdk-a", "a")
	var main, a *string
	if err := pool.QueryRow(ctx, `SELECT agent_session_id FROM sessions WHERE id = $1`, id).Scan(&main); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT agent_session_id FROM session_threads WHERE session_id = $1 AND thread_key = 'a'`, id).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if main == nil || *main != "sdk-main-2" || a == nil || *a != "sdk-a" {
		t.Fatalf("main %v, a %v", main, a)
	}
}

// docs/adr/0084 §4: a crashed pod's head entry is closed with a crash result once an effect of it ran, or on its
// third crash (and then the owner is alerted); the result moves the thread's cursor past it.
func TestCrash_SettlesWithACrashResultAndAlertsOnPoison(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	ctx := context.Background()
	var alerts []string
	s.SetNotifyBlockedFunc(func(_ context.Context, sessionID, _, _, summary string) error {
		alerts = append(alerts, sessionID+": "+summary)
		return nil
	})
	thread(t, pool, id, "fx", 0)
	thread(t, pool, id, "poison", 0)
	put(t, pool, id, 1, "fx", "human", "discussion", "send it")
	put(t, pool, id, 2, "fx", "agent", "assistant", "working")
	put(t, pool, id, 3, "poison", "human", "discussion", "boom")
	put(t, pool, id, 4, "poison", "agent", "assistant", "working")
	if err := store.NoteEffect(ctx, id, "fx", 1); err != nil {
		t.Fatal(err)
	}
	crash := func() {
		t.Helper()
		if err := s.applyPodEvent(ctx, &agentfleetv1.PodEvent{SessionId: id, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER, Phase: agentfleetv1.PodPhase_POD_PHASE_CRASHED}); err != nil {
			t.Fatal(err)
		}
	}
	cursors := func() map[string]int64 {
		t.Helper()
		th, err := store.Threads(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int64{}
		for _, x := range th {
			m[x.Key] = x.Cursor
		}
		return m
	}
	crash()
	if c := cursors(); c["fx"] != 2 || c["poison"] != 0 || len(alerts) != 0 {
		t.Fatalf("after crash 1: cursors %v alerts %v; the effect-run entry should be done, poison not yet", c, alerts)
	}
	// The same crash reported again (a provisioner restart, a re-warm that failed before dispatch) is not a second
	// attempt: no agent output above the last settle.
	crash()
	crash()
	if c := cursors(); c["poison"] != 0 || len(alerts) != 0 {
		t.Fatalf("repeated reports of one crash: cursors %v alerts %v; poison was counted twice", c, alerts)
	}
	put(t, pool, id, 50, "poison", "agent", "assistant", "working again")
	crash()
	put(t, pool, id, 51, "poison", "agent", "assistant", "and again")
	crash()
	if c := cursors(); c["poison"] != 4 || len(alerts) != 1 {
		t.Fatalf("after crash 3: cursors %v alerts %v; the poison entry should be done and alerted", c, alerts)
	}
	// Replayed events settle nothing twice: the cursors have moved past both heads.
	crash()
	if len(alerts) != 1 {
		t.Fatalf("alerts after a fourth crash: %v", alerts)
	}
	var thr, text string
	if err := pool.QueryRow(ctx, `SELECT thread_key, text FROM transcript WHERE session_id = $1 AND type = 'result' AND thread_key = 'poison'`, id).Scan(&thr, &text); err != nil {
		t.Fatal(err)
	}
	if text == "" {
		t.Fatal("no crash result")
	}
	if _, ok, _ := store.ResumeFrom(ctx, id); ok {
		t.Fatal("a settled entry is still waiting for the re-warm")
	}
}

// docs/adr/0084 §4: publish_event and draft_card note, before they run, the entry their effect key names.
func TestEffectMethods_NoteTheirEntry(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	ctx := context.Background()
	thread(t, pool, id, "t", 0)
	s.SetPublishFunc(func(context.Context, string, string, string, string, string, string) (string, bool, int32, error) {
		return "e1", true, 1, nil
	})
	s.SetDecisions(decisions.NewStore(pool))
	effect := func() int64 {
		t.Helper()
		var seq *int64
		if err := pool.QueryRow(ctx, `SELECT effect_seq FROM session_threads WHERE session_id = $1 AND thread_key = 't'`, id).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq == nil {
			return -1
		}
		return *seq
	}
	if _, err := s.PublishEvent(ctx, &agentfleetv1.PublishEventRequest{SessionId: id, Type: "dev.bnei.agent.x", Subject: "s", Text: "t", IdempotencyKey: "plain"}); err != nil {
		t.Fatal(err)
	}
	if effect() != -1 {
		t.Fatal("a key that is not an effect key was noted")
	}
	if _, err := s.PublishEvent(ctx, &agentfleetv1.PublishEventRequest{SessionId: id, Type: "dev.bnei.agent.x", Subject: "s", Text: "t", IdempotencyKey: sessions.EffectKey(id, "t", 5, 1)}); err != nil {
		t.Fatal(err)
	}
	if effect() != 5 {
		t.Fatalf("publish_event noted %d, want 5", effect())
	}
	card := &agentfleetv1.DraftCardRequest{SessionId: id, Ask: "Merge it?", IdempotencyKey: sessions.EffectKey(id, "t", 9, 1),
		Subject: &agentfleetv1.CardSubject{Repo: "o/r", Kind: "pr", Number: 7},
		Options: []*agentfleetv1.CardOption{
			{Label: "Yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}, Reversible: "yes"},
			{Label: "No", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}, Reversible: "yes"},
		}}
	if _, err := s.DraftCard(metadataThread(ctx, "t"), card); err != nil {
		t.Fatal(err)
	}
	if effect() != 9 {
		t.Fatalf("draft_card noted %d, want 9", effect())
	}
	// memory_write: core's grant is the effect's marker (the write follows it in the sidecar).
	if _, err := s.AuthorizeMemoryWrite(ctx, &agentfleetv1.AuthorizeMemoryWriteRequest{SessionId: id, IdempotencyKey: sessions.EffectKey(id, "t", 12, 1)}); err != nil {
		t.Fatal(err)
	}
	if effect() != 12 {
		t.Fatalf("memory_write noted %d, want 12", effect())
	}
	_ = store
}

// submit_review posts to GitHub through the provisioner: the entry is marked before the post, so a crash after
// it never replays the review. The provisioner here is unreachable, which fails the post and still shows the mark.
func TestSubmitReview_NotesItsEntryBeforePosting(t *testing.T) {
	s, _, pool, id := newCharacter(t)
	ctx := context.Background()
	thread(t, pool, id, "t", 0)
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET effective = '{"sidecarTools":["submit_review"],"mode":{"default":"default","allow":["default"]},
		"credentials":{"vault":"v","services":[{"host":"github.com","paths":["/o/r.git/*"],"secret":"x"}]}}'::jsonb WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	// A real reviewer is a single-shot session on the PR's thread; the character stands in for it here, so the
	// standing row's principal constraint (its thread is main) is dropped from this test's database.
	if _, err := pool.Exec(ctx, `ALTER TABLE sessions DROP CONSTRAINT sessions_standing_principal`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET thread_key = 'github/o/r/pr/1' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	prov, err := provisionerclient.New("127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	s.provisioner = prov
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = s.SubmitReview(cctx, &agentfleetv1.SubmitReviewRequest{
		SessionId: id, Repo: "o/r", Pr: 1, HeadSha: strings.Repeat("a", 40), Verdict: "comment", Body: "looks fine",
		IdempotencyKey: sessions.EffectKey(id, "t", 14, 1),
	})
	if err == nil {
		t.Fatal("the unreachable provisioner posted a review")
	}
	var seq *int64
	if err := pool.QueryRow(ctx, `SELECT effect_seq FROM session_threads WHERE session_id = $1 AND thread_key = 't'`, id).Scan(&seq); err != nil || seq == nil || *seq != 14 {
		t.Fatalf("submit_review noted %v (%v), want 14: the entry would replay the review after a crash", seq, err)
	}
}

func metadataThread(ctx context.Context, th string) context.Context {
	return withThread(metadata.NewIncomingContext(ctx, metadata.Pairs(mdThread, th)))
}
