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

// docs/adr/0084 amendment A4: Stop keeps every waiting entry, so a resumed stream can start below the abort that stopped
// the earlier pod. That abort, older than the new pod, must not end it; one written after the pod started does.
func TestStreamHumanMessages_StandingSkipsAnAbortFromBeforeThePod(t *testing.T) {
	s, _, pool, id := newCharacter(t)
	ctx := context.Background()
	put(t, pool, id, 1, "main", "human", "discussion", "waiting")
	put(t, pool, id, 2, "main", "human", "abort", "stopped by root") // the earlier pod's Stop
	put(t, pool, id, 3, "main", "human", "abort", "stopped by root") // this pod's
	for _, q := range []string{`UPDATE sessions SET pod_started_at = now() WHERE id = $1`,
		`UPDATE transcript SET created_at = now() - interval '1 hour' WHERE session_id = $1 AND seq <= 2`,
		`UPDATE transcript SET created_at = now() + interval '1 minute' WHERE session_id = $1 AND seq = 3`} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	_, sha, err := s.agentSpecOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()
	rec := &streamRecorder{ctx: sctx}
	_ = s.StreamHumanMessages(&agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha}, rec)
	if want := []string{"main:waiting", "main:stopped by root"}; !slices.Equal(rec.texts, want) {
		t.Fatalf("streamed %v, want the waiting entry and only this pod's abort %v", rec.texts, want)
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
	// Threads other than main that have one, a parked one too (docs/adr/0084 amendment A4: the owner's next message
	// resumes it); main resumes from the row's own id.
	if got := res.GetThreadSessionIds(); len(got) != 2 || got["a"] != "sdk-a" || got["b"] != "sdk-b" {
		t.Fatalf("thread session ids = %v, want a and the parked b", got)
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
		if strings.Contains(summary, "skipped") { // the poison alert; the crash-loop alert has its own test
			alerts = append(alerts, sessionID+": "+summary)
		}
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
	crash := func(pod string) {
		t.Helper()
		if err := s.applyPodEvent(ctx, &agentfleetv1.PodEvent{SessionId: id, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER, Phase: agentfleetv1.PodPhase_POD_PHASE_CRASHED, PodName: "worker-x", JobUid: pod}); err != nil {
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
	crash("p1")
	if c := cursors(); c["fx"] != 2 || c["poison"] != 0 || len(alerts) != 0 {
		t.Fatalf("after crash 1: cursors %v alerts %v; the effect-run entry should be done, poison not yet", c, alerts)
	}
	// The same crash reported again (a provisioner restart, a re-warm that failed before dispatch) is not a second
	// attempt: no agent output above the last settle.
	crash("p1")
	crash("p1")
	if c := cursors(); c["poison"] != 0 || len(alerts) != 0 {
		t.Fatalf("repeated reports of one crash: cursors %v alerts %v; poison was counted twice", c, alerts)
	}
	put(t, pool, id, 60, "poison", "agent", "assistant", "working again")
	crash("p2")
	put(t, pool, id, 70, "poison", "agent", "assistant", "and again")
	crash("p3")
	if c := cursors(); c["poison"] != 4 || len(alerts) != 1 {
		t.Fatalf("after crash 3: cursors %v alerts %v; the poison entry should be done and alerted", c, alerts)
	}
	// Replayed events settle nothing twice: the cursors have moved past both heads.
	crash("p3")
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

// crashEvent is what the provisioner reports: the Job name is the session's, fixed across its pods; the UID is the pod's.
func crashEvent(id, uid, msg string) *agentfleetv1.PodEvent {
	return &agentfleetv1.PodEvent{SessionId: id, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER, Phase: agentfleetv1.PodPhase_POD_PHASE_CRASHED, PodName: "worker-fixed", JobUid: uid, Message: msg}
}

func crashEntries(t *testing.T, pool *pgxpool.Pool, id string) (n int, threads []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT thread_key FROM transcript WHERE session_id = $1 AND "from" = 'fleet' AND type = 'system'
		AND text LIKE $2 ORDER BY seq`, id, sessions.CrashEntryPrefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var th string
		if err := rows.Scan(&th); err != nil {
			t.Fatal(err)
		}
		threads = append(threads, th)
	}
	return len(threads), threads
}

// docs/adr/0084 §3: every CRASHED path appends a crash entry with the pod's reason, to the thread whose turn was
// running (main when none was); the same crash seen twice is one entry, two crashes of one Job name are two;
// Evicted and Preempted (the event's termination reason) get none; the third
// in an hour alerts the owner, and the re-warm stops.
func TestCrash_AppendsACrashEntryPerPod(t *testing.T) {
	ctx := context.Background()
	s, store, pool, id := newCharacter(t)
	var alerts []string
	s.SetNotifyBlockedFunc(func(_ context.Context, _, _, _, summary string) error {
		alerts = append(alerts, summary)
		return nil
	})
	thread(t, pool, id, "busy", 0)
	put(t, pool, id, 1, "busy", "human", "discussion", "go")
	put(t, pool, id, 2, "busy", "agent", "assistant", "working")
	apply := func(pod, msg string) {
		t.Helper()
		if err := s.applyPodEvent(ctx, crashEvent(id, pod, msg)); err != nil {
			t.Fatal(err)
		}
	}
	apply("p1", "OOMKilled")
	apply("p1", "OOMKilled") // re-sighting after a core restart
	if n, th := crashEntries(t, pool, id); n != 1 || th[0] != "busy" {
		t.Fatalf("entries %d in %v; want one, in the thread whose turn was running", n, th)
	}
	var text string
	if err := pool.QueryRow(ctx, `SELECT text FROM transcript WHERE session_id = $1 AND text LIKE $2`, id, sessions.CrashEntryPrefix+"%").Scan(&text); err != nil || !strings.Contains(text, "OOMKilled") {
		t.Fatalf("the entry must carry the pod's reason: %q %v", text, err)
	}
	for _, loss := range []string{"Evicted", "Preempted"} {
		ev := crashEvent(id, "u-"+loss, "worker job reached a terminal Failed phase")
		ev.TerminationReason = loss
		if err := s.applyPodEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := crashEntries(t, pool, id); n != 1 {
		t.Fatalf("a node taking the pod away is not a crash: %d entries", n)
	}
	apply("p4", "Error") // nothing was running: main
	if n, th := crashEntries(t, pool, id); n != 2 || th[1] != "main" {
		t.Fatalf("entries %d in %v; want the second in main", n, th)
	}
	if len(alerts) != 0 {
		t.Fatalf("alerted before the third crash: %v", alerts)
	}
	apply("p5", "Error")
	if len(alerts) != 1 {
		t.Fatalf("the third crash in an hour must alert the owner once: %v", alerts)
	}
	if n, err := store.CrashesInWindow(ctx, id); err != nil || n != 3 {
		t.Fatalf("window = %d, %v", n, err)
	}
	if ids, err := store.ListRewarmStanding(ctx); err != nil || slices.Contains(ids, id) {
		t.Fatalf("three crashes must stop the re-warm: %v %v", ids, err)
	}
}

// A drain stops intake of new turns on every thread of a live pod; answers and aborts pass (docs/adr/0084 §3).
func TestStreamHumanMessages_DrainHoldsNewTurnsBack(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	thread(t, pool, id, "a", 0)
	put(t, pool, id, 1, "a", "human", "discussion", "new turn")
	put(t, pool, id, 2, "a", "human", "answer", "an answer")
	put(t, pool, id, 3, "a", "human", "abort", "stop")
	if _, err := store.Drain(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	_, sha, err := s.agentSpecOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rec := &streamRecorder{ctx: ctx}
	_ = s.StreamHumanMessages(&agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha}, rec)
	if !slices.Equal(rec.texts, []string{"a:an answer", "a:stop"}) {
		t.Fatalf("delivered %v; want the answer and the abort, not the new turn", rec.texts)
	}
}

// A planned loss adds no crash entry and counts no poison crash, but still closes a head whose effect ran.
func TestCrash_PlannedLossStillSettlesAnEffectHead(t *testing.T) {
	ctx := context.Background()
	s, store, pool, id := newCharacter(t)
	thread(t, pool, id, "fx", 0)
	thread(t, pool, id, "poison", 0)
	put(t, pool, id, 1, "fx", "human", "discussion", "send it")
	put(t, pool, id, 2, "fx", "agent", "assistant", "working")
	put(t, pool, id, 3, "poison", "human", "discussion", "boom")
	put(t, pool, id, 4, "poison", "agent", "assistant", "working")
	if err := store.NoteEffect(ctx, id, "fx", 1); err != nil {
		t.Fatal(err)
	}
	ev := crashEvent(id, "u1", "worker job reached a terminal Failed phase")
	ev.TerminationReason = "Evicted"
	if err := s.applyPodEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	th, err := store.Threads(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cur := map[string]int64{}
	for _, x := range th {
		cur[x.Key] = x.Cursor
	}
	if cur["fx"] != 2 || cur["poison"] != 0 {
		t.Fatalf("cursors %v: the effect head must close, the other stay", cur)
	}
	if n, _ := crashEntries(t, pool, id); n != 0 {
		t.Fatalf("a planned loss appended %d crash entries", n)
	}
	var crashes int
	if err := pool.QueryRow(ctx, `SELECT crashes FROM session_threads WHERE session_id = $1 AND thread_key = 'poison'`, id).Scan(&crashes); err != nil || crashes != 0 {
		t.Fatalf("poison count after an eviction = %d, %v", crashes, err)
	}
}

// A drain that ends without a new pod (a revert) delivers what it held back to the live pod.
func TestStreamHumanMessages_DrainEndDeliversHeldEntries(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	thread(t, pool, id, "a", 0)
	put(t, pool, id, 1, "a", "human", "discussion", "held turn")
	if _, err := store.Drain(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, sha, err := s.agentSpecOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rec := &streamRecorder{ctx: ctx}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = store.EndDrain(context.Background(), id)
	}()
	done := make(chan struct{})
	go func() {
		_ = s.StreamHumanMessages(&agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha}, rec)
		close(done)
	}()
	time.Sleep(1800 * time.Millisecond)
	cancel()
	<-done
	if !slices.Equal(rec.texts, []string{"a:held turn"}) {
		t.Fatalf("delivered %v; the held turn must reach the pod when the drain ends", rec.texts)
	}
}

// docs/adr/0084 §9: past its day's turns a character's thread waits at its head for what is not the owner's, and
// nothing jumps a held entry; the owner's own message, an answer and an abort pass a thread with nothing held. A
// raise_budget override lifts the hold.
func TestStreamHumanMessages_BudgetHold(t *testing.T) {
	s, store, pool, id := newCharacter(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET effective = jsonb_set(effective, '{limits}', '{"turnsPerDay":2}') WHERE name = 'a'`); err != nil {
		t.Fatal(err)
	}
	thread(t, pool, id, "a", 0)
	thread(t, pool, id, "b", 0)
	put(t, pool, id, 1, "main", "worker", "result", `{"subtype":"success","consumedSeq":0}`)
	put(t, pool, id, 2, "main", "worker", "result", `{"subtype":"success","consumedSeq":0}`)
	put(t, pool, id, 3, "main", "fleet", "result", `{"subtype":"success","crash":true}`) // a crash is no turn
	put(t, pool, id, 4, "a", "session", "discussion", "peer head")
	put(t, pool, id, 5, "a", "human", "discussion", "owner behind")
	put(t, pool, id, 6, "a", "human", "answer", "an answer")
	put(t, pool, id, 7, "b", "human", "discussion", "owner on b")
	put(t, pool, id, 8, "b", "session", "discussion", "peer on b")
	put(t, pool, id, 9, "b", "human", "discussion", "owner after peer")

	_, sha, err := s.agentSpecOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stream := func() []string {
		c, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
		defer cancel()
		rec := &streamRecorder{ctx: c}
		_ = s.StreamHumanMessages(&agentfleetv1.StreamHumanMessagesRequest{SessionId: id, AgentSpecSha: sha}, rec)
		return rec.texts
	}
	want := []string{"a:an answer", "b:owner on b"}
	if got := stream(); !slices.Equal(got, want) {
		t.Fatalf("held: streamed %v, want %v", got, want)
	}
	if err := store.RaiseBudget(ctx, id); err != nil {
		t.Fatal(err)
	}
	want = []string{"a:peer head", "a:owner behind", "a:an answer", "b:owner on b", "b:peer on b", "b:owner after peer"}
	if got := stream(); !slices.Equal(got, want) {
		t.Fatalf("overridden: streamed %v, want %v", got, want)
	}
}
