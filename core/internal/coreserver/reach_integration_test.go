//go:build integration

package coreserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// reachCharacter is a started character whose pinned spec is effective, and a repo session beside it.
func reachCharacter(t *testing.T, effective string) (*Server, string, string) {
	t.Helper()
	s, id, repoID, _ := reachCharacterDB(t, effective)
	return s, id, repoID
}

func reachCharacterDB(t *testing.T, effective string) (*Server, string, string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('a', 'sha-a', '{}', $1, true)`, effective); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, sessions.CreateParams{
		Repo: "r", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a",
		Owner: "client:standing:a", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:a", ThreadKey: "main", StartedBy: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{sessions: store, transcr: transcript.NewPostgresStore(pool, decisions.OnAppend)}, id, repoID, pool
}

func code(err error) codes.Code { return status.Code(err) }

type fakeSpawner struct {
	def, input, key string
	err             error
}

func (f *fakeSpawner) SpawnWorker(_ context.Context, _, def, input, key string) (string, error) {
	f.def, f.input, f.key = def, input, key
	return "thread-1", f.err
}

func (f *fakeSpawner) SpawnResult(context.Context, string, string, time.Duration) (*agentfleetv1.WaitForResultResponse, error) {
	return &agentfleetv1.WaitForResultResponse{State: "done"}, f.err
}

const reachSpec = `{"sidecarTools":["spawn_worker","wait_for_result","open_issue","list_responder_cards","view_logs"],
	"spawns":{"allow":["worker"]},"owns":{"repos":["MohammadBnei/notes"]},"cluster":{"logNamespaces":["agent-fleet"]},
	"mode":{"default":"default","allow":["default"]}}`

// docs/adr/0084 §6: SpawnWorker is granted per definition and only for spawns.allow, and core's own refusals
// (the sub-pool, the hourly cap) reach the character as their gRPC code.
func TestSpawnWorker_GrantAndAllowList(t *testing.T) {
	ctx := context.Background()
	s, id, _ := reachCharacter(t, reachSpec)
	sp := &fakeSpawner{}
	s.SetSpawner(sp)
	req := func(def string) *agentfleetv1.SpawnWorkerRequest {
		return &agentfleetv1.SpawnWorkerRequest{SessionId: id, Definition: def, Input: "go", IdempotencyKey: "k"}
	}
	res, err := s.SpawnWorker(ctx, req("worker"))
	if err != nil || res.GetThreadId() != "thread-1" || sp.def != "worker" || sp.input != "go" || sp.key != "k" {
		t.Fatalf("spawn: %v %v %+v", res, err, sp)
	}
	sp.def = ""
	if _, err := s.SpawnWorker(ctx, req("other")); code(err) != codes.PermissionDenied || sp.def != "" {
		t.Errorf("a definition off spawns.allow: %v (spawner called: %q)", err, sp.def)
	}
	if _, err := s.SpawnWorker(ctx, &agentfleetv1.SpawnWorkerRequest{SessionId: id, Definition: "worker", IdempotencyKey: "k"}); code(err) != codes.InvalidArgument {
		t.Errorf("no input: %v", err)
	}
	sp.err = errors.New("plain")
	if _, err := s.SpawnWorker(ctx, req("worker")); err == nil {
		t.Error("a spawner failure must reach the caller")
	}
	// Without the grant in sidecarTools the pod gets nothing, whatever spawns.allow says.
	s2, id2, repo2 := reachCharacter(t, `{"sidecarTools":["send_message"],"spawns":{"allow":["worker"]}}`)
	s2.SetSpawner(&fakeSpawner{})
	if _, err := s2.SpawnWorker(ctx, &agentfleetv1.SpawnWorkerRequest{SessionId: id2, Definition: "worker", Input: "x", IdempotencyKey: "k"}); code(err) != codes.PermissionDenied {
		t.Errorf("no spawn_worker grant: %v", err)
	}
	if _, err := s2.WaitForSpawn(ctx, &agentfleetv1.WaitForSpawnRequest{SessionId: id2, ThreadId: "t"}); code(err) != codes.PermissionDenied {
		t.Errorf("no wait_for_result grant: %v", err)
	}
	// A repo session is no definition session.
	if _, err := s2.SpawnWorker(ctx, &agentfleetv1.SpawnWorkerRequest{SessionId: repo2, Definition: "worker", Input: "x", IdempotencyKey: "k"}); code(err) != codes.PermissionDenied {
		t.Errorf("a repo session: %v", err)
	}
	sp.err = nil
	if _, err := s.WaitForSpawn(ctx, &agentfleetv1.WaitForSpawnRequest{SessionId: id, ThreadId: "t"}); err != nil {
		t.Errorf("wait: %v", err)
	}
}

// docs/adr/0084 §4: a create the provisioner answers Unavailable for may have landed, and the sidecar retries Unavailable,
// so core must not pass that code on.
func TestOpenIssue_UnavailableIsNotRetryable(t *testing.T) {
	s, id, _ := reachCharacter(t, reachSpec)
	calls := 0
	s.SetIssueFunc(func(context.Context, *agentfleetv1.CreateIssueRequest) (*agentfleetv1.CreateIssueResponse, error) {
		calls++
		return nil, status.Error(codes.Unavailable, "read: connection reset")
	})
	_, err := s.OpenIssue(context.Background(), &agentfleetv1.OpenIssueRequest{SessionId: id, Repo: "MohammadBnei/notes", Title: "T", IdempotencyKey: "k"})
	if c := status.Code(err); c == codes.Unavailable || c == codes.Canceled || c == codes.OK || calls != 1 {
		t.Fatalf("got %v after %d calls, want a non-retryable status", err, calls)
	}
}

// docs/adr/0084 §6: OpenIssue is limited to owns.repos, and the provisioner is reached only after that check.
func TestOpenIssue_OwnedReposOnly(t *testing.T) {
	ctx := context.Background()
	s, id, _ := reachCharacter(t, reachSpec)
	var got *agentfleetv1.CreateIssueRequest
	s.SetIssueFunc(func(_ context.Context, r *agentfleetv1.CreateIssueRequest) (*agentfleetv1.CreateIssueResponse, error) {
		got = r
		return &agentfleetv1.CreateIssueResponse{Url: "https://github.com/MohammadBnei/notes/issues/1"}, nil
	})
	req := func(mut func(*agentfleetv1.OpenIssueRequest)) *agentfleetv1.OpenIssueRequest {
		r := &agentfleetv1.OpenIssueRequest{SessionId: id, Repo: "MohammadBnei/notes", Title: "Broken", Body: "b", IdempotencyKey: "k"}
		if mut != nil {
			mut(r)
		}
		return r
	}
	res, err := s.OpenIssue(ctx, req(nil))
	if err != nil || res.GetUrl() == "" || got.GetRepo() != "MohammadBnei/notes" || got.GetTitle() != "Broken" {
		t.Fatalf("open: %v %v %v", res, err, got)
	}
	got = nil
	if _, err := s.OpenIssue(ctx, req(func(r *agentfleetv1.OpenIssueRequest) { r.Repo = "mohammadbnei/NOTES" })); err != nil || got == nil {
		t.Errorf("GitHub names fold case: %v", err)
	}
	got = nil
	for name, tc := range map[string]struct {
		mut  func(*agentfleetv1.OpenIssueRequest)
		want codes.Code
	}{
		"not owned": {func(r *agentfleetv1.OpenIssueRequest) { r.Repo = "MohammadBnei/agent-fleet" }, codes.PermissionDenied},
		"bad repo":  {func(r *agentfleetv1.OpenIssueRequest) { r.Repo = "no-slash" }, codes.InvalidArgument},
		"dotdot":    {func(r *agentfleetv1.OpenIssueRequest) { r.Repo = "../x" }, codes.PermissionDenied},
		"no title":  {func(r *agentfleetv1.OpenIssueRequest) { r.Title = "" }, codes.InvalidArgument},
		"multiline": {func(r *agentfleetv1.OpenIssueRequest) { r.Title = "a\nb" }, codes.InvalidArgument},
		"huge body": {func(r *agentfleetv1.OpenIssueRequest) { r.Body = strings.Repeat("x", maxIssueBody+1) }, codes.InvalidArgument},
	} {
		if _, err := s.OpenIssue(ctx, req(tc.mut)); code(err) != tc.want {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if got != nil {
		t.Errorf("a refused issue reached the provisioner: %v", got)
	}
	s3, id3, _ := reachCharacter(t, `{"sidecarTools":["send_message"],"owns":{"repos":["MohammadBnei/notes"]}}`)
	s3.SetIssueFunc(func(context.Context, *agentfleetv1.CreateIssueRequest) (*agentfleetv1.CreateIssueResponse, error) {
		t.Fatal("no grant, no provisioner call")
		return nil, nil
	})
	if _, err := s3.OpenIssue(ctx, &agentfleetv1.OpenIssueRequest{SessionId: id3, Repo: "MohammadBnei/notes", Title: "T"}); code(err) != codes.PermissionDenied {
		t.Errorf("no open_issue grant: %v", err)
	}
}

func TestListResponderCards_BoundsAndGrant(t *testing.T) {
	ctx := context.Background()
	s, id, _ := reachCharacter(t, reachSpec)
	var gotSince time.Time
	var gotLimit int
	s.SetResponderCards(func(_ context.Context, since time.Time, limit int) ([]*agentfleetv1.ResponderCard, error) {
		gotSince, gotLimit = since, limit
		return []*agentfleetv1.ResponderCard{{Id: "c"}}, nil
	})
	if res, err := s.ListResponderCards(ctx, &agentfleetv1.ListResponderCardsRequest{SessionId: id}); err != nil || len(res.GetCards()) != 1 ||
		gotLimit != defaultCardsLimit || time.Since(gotSince) < defaultCardsSince-time.Minute || time.Since(gotSince) > defaultCardsSince+time.Minute {
		t.Fatalf("defaults: %v %v since %v limit %d", res, err, gotSince, gotLimit)
	}
	if _, err := s.ListResponderCards(ctx, &agentfleetv1.ListResponderCardsRequest{SessionId: id, Since: "2026-01-02T03:04:05Z", Limit: 9999}); err != nil ||
		gotLimit != maxCardsLimit || !gotSince.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("explicit: %v since %v limit %d", err, gotSince, gotLimit)
	}
	if _, err := s.ListResponderCards(ctx, &agentfleetv1.ListResponderCardsRequest{SessionId: id, Since: "yesterday"}); code(err) != codes.InvalidArgument {
		t.Errorf("bad since: %v", err)
	}
	s2, id2, _ := reachCharacter(t, `{"sidecarTools":["send_message"]}`)
	s2.SetResponderCards(func(context.Context, time.Time, int) ([]*agentfleetv1.ResponderCard, error) {
		t.Fatal("no grant, no read")
		return nil, nil
	})
	if _, err := s2.ListResponderCards(ctx, &agentfleetv1.ListResponderCardsRequest{SessionId: id2}); code(err) != codes.PermissionDenied {
		t.Errorf("no grant: %v", err)
	}
}

// docs/adr/0084 §6: view_logs for a definition session needs the grant and a namespace on cluster.logNamespaces,
// and says nothing about a repo session's or the provisioner's.
func TestViewLogs_DefinitionNamespaceScope(t *testing.T) {
	s, id, repoID := reachCharacter(t, reachSpec)
	var queried []string
	s.loki = &mockLokiClient{queryFunc: func(_ context.Context, r lokiclient.QueryRequest) ([]lokiclient.LogEntry, error) {
		queried = append(queried, r.Namespace)
		return nil, nil
	}}
	as := func(session string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(mdSessionID, session))
	}
	view := func(ctx context.Context, ns string) error {
		_, err := s.ViewLogs(ctx, &agentfleetv1.ViewLogsRequest{Component: "app", Namespace: ns})
		return err
	}
	if err := view(as(id), "agent-fleet"); err != nil {
		t.Fatalf("a listed namespace: %v", err)
	}
	for _, ns := range []string{"kube-system", "", "agent-fleet-2"} {
		if err := view(as(id), ns); code(err) != codes.PermissionDenied {
			t.Errorf("namespace %q: %v, want PermissionDenied", ns, err)
		}
	}
	if len(queried) != 1 {
		t.Errorf("a refused read reached Loki: %v", queried)
	}
	s2, id2, _ := reachCharacter(t, `{"sidecarTools":["send_message"],"cluster":{"logNamespaces":["agent-fleet"]}}`)
	s2.loki = s.loki
	if err := func() error {
		_, err := s2.ViewLogs(metadata.NewIncomingContext(context.Background(), metadata.Pairs(mdSessionID, id2)), &agentfleetv1.ViewLogsRequest{Component: "app", Namespace: "agent-fleet"})
		return err
	}(); code(err) != codes.PermissionDenied {
		t.Errorf("a namespace without the view_logs grant: %v", err)
	}
	// A repo session and the provisioner (no session) keep the unscoped read, default namespace included.
	if err := view(as(repoID), "kube-system"); err != nil {
		t.Errorf("a repo session is unchanged: %v", err)
	}
	if err := view(context.Background(), ""); err != nil {
		t.Errorf("the provisioner is unchanged: %v", err)
	}
}

type fakeSpeaker struct {
	text, face, key string
	err             error
}

func (f *fakeSpeaker) Speak(_ context.Context, _, text, face, key string) (bool, error) {
	f.text, f.face, f.key = text, face, key
	return f.err == nil, f.err
}

// docs/adr/0084 §7: Speak is granted per definition, reaches only the speaker (the owner's browser), and a refusal
// of core's own (the per-turn bound) reaches the character as its gRPC code.
func TestSpeak_Grant(t *testing.T) {
	ctx := context.Background()
	s, id, _, pool := reachCharacterDB(t, `{"sidecarTools":["speak"],"mode":{"default":"default","allow":["default"]}}`)
	sp := &fakeSpeaker{}
	s.SetSpeaker(sp)
	res, err := s.Speak(ctx, &agentfleetv1.SpeakRequest{SessionId: id, Text: "hi", Face: "happy", IdempotencyKey: "k"})
	if err != nil || !res.GetDelivered() || sp.text != "hi" || sp.face != "happy" || sp.key != "k" {
		t.Fatalf("speak: %v %v %+v", res, err, sp)
	}
	// A line is live-only and safe to replay: it never marks the turn's entry as "an effect ran" (docs/adr/0084 §4).
	if _, err := s.Speak(ctx, &agentfleetv1.SpeakRequest{SessionId: id, Text: "again", IdempotencyKey: sessions.EffectKey(id, "main", 5, 1)}); err != nil {
		t.Fatal(err)
	}
	var noted *int64
	if err := pool.QueryRow(ctx, `SELECT effect_seq FROM session_threads WHERE session_id = $1 AND thread_key = 'main'`, id).Scan(&noted); err != nil || noted != nil {
		t.Errorf("a spoken line noted an effect: %v %v", noted, err)
	}
	sp.err = status.Error(codes.ResourceExhausted, "too many")
	if _, err := s.Speak(ctx, &agentfleetv1.SpeakRequest{SessionId: id, Text: "hi", IdempotencyKey: "k2"}); code(err) != codes.ResourceExhausted {
		t.Errorf("a refusal of core's own: %v", err)
	}
	s2, id2, _ := reachCharacter(t, `{"sidecarTools":["send_message"]}`)
	sp2 := &fakeSpeaker{}
	s2.SetSpeaker(sp2)
	if _, err := s2.Speak(ctx, &agentfleetv1.SpeakRequest{SessionId: id2, Text: "hi", IdempotencyKey: "k"}); code(err) != codes.PermissionDenied || sp2.text != "" {
		t.Errorf("no grant, no line: %v %q", err, sp2.text)
	}
}

// ADR-0086 §2: kubectl_read asks core first. It needs cluster.read on the pinned definition and B in the stored set;
// a repo session and a missing session are refused.
func TestAuthorizeKubeRead_NeedsGrantAndB(t *testing.T) {
	ctx := context.Background()
	s, id, repoID, pool := reachCharacterDB(t, `{"cluster":{"read":true,"logNamespaces":["agent-fleet"]},"mode":{"default":"default","allow":["default"]}}`)
	ask := func(sid string) error {
		_, err := s.AuthorizeKubeRead(ctx, &agentfleetv1.AuthorizeKubeReadRequest{SessionId: sid})
		return err
	}
	if err := ask(id); err != nil {
		t.Fatalf("granted, BC: %v", err)
	}
	if code(ask(repoID)) != codes.PermissionDenied {
		t.Error("a repo session must be refused")
	}
	if code(ask("00000000-0000-0000-0000-000000000000")) != codes.NotFound {
		t.Error("unknown session must be NotFound")
	}
	noB, err := sessions.NewStore(pool).Create(ctx, sessions.CreateParams{
		Repo: "r", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a", TrustSet: "AC",
		Owner: "client:bridge", Visibility: "private", Invoker: "bridge", ThreadKey: "x", StartedBy: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code(ask(noB)) != codes.PermissionDenied {
		t.Error("a set without B must be refused")
	}
	s2, id2, _ := reachCharacter(t, `{"cluster":{"logNamespaces":["agent-fleet"]},"mode":{"default":"default","allow":["default"]}}`)
	if _, err := s2.AuthorizeKubeRead(ctx, &agentfleetv1.AuthorizeKubeReadRequest{SessionId: id2}); code(err) != codes.PermissionDenied {
		t.Errorf("no cluster.read grant: %v", err)
	}
}

type fakeMetrics struct {
	q          string
	start, end time.Time
	step       time.Duration
	instant    bool
}

func (f *fakeMetrics) Query(_ context.Context, q string) (json.RawMessage, error) {
	f.q, f.instant = q, true
	return json.RawMessage(`{"resultType":"vector"}`), nil
}

func (f *fakeMetrics) QueryRange(_ context.Context, q string, start, end time.Time, step time.Duration) (json.RawMessage, error) {
	f.q, f.start, f.end, f.step, f.instant = q, start, end, step, false
	return json.RawMessage(`{"resultType":"matrix"}`), nil
}

// docs/adr/0084 §6: query_metrics needs the grant, runs instant or range, and bounds the range and the step.
func TestQueryMetrics_GrantAndBounds(t *testing.T) {
	ctx := context.Background()
	s, id, _ := reachCharacter(t, `{"sidecarTools":["query_metrics"]}`)
	f := &fakeMetrics{}
	s.SetMetrics(f)
	if res, err := s.QueryMetrics(ctx, &agentfleetv1.CoreQueryMetricsRequest{SessionId: id, Query: "up"}); err != nil || !f.instant || f.q != "up" || !strings.Contains(res.GetResultJson(), "vector") {
		t.Fatalf("instant: %v %v %+v", res, err, f)
	}
	now := time.Now().UTC()
	rng := func(start, end time.Time, step int32) error {
		_, err := s.QueryMetrics(ctx, &agentfleetv1.CoreQueryMetricsRequest{SessionId: id, Query: "up", Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339), StepSeconds: step})
		return err
	}
	if err := rng(now.Add(-time.Hour), now, 0); err != nil || f.instant || f.step != time.Minute {
		t.Fatalf("range default step: %v %+v", err, f)
	}
	if err := rng(now.Add(-time.Hour), now, 1); err != nil || f.step != minMetricsStep {
		t.Errorf("step floor: %v %v", err, f.step)
	}
	if err := rng(now.Add(-24*time.Hour), now, 15); err != nil || f.end.Sub(f.start)/f.step > maxMetricsPoints {
		t.Errorf("point cap: %v %+v", err, f)
	}
	if err := rng(now.Add(-25*time.Hour), now, 60); code(err) != codes.InvalidArgument {
		t.Errorf("range over 24h: %v", err)
	}
	if err := rng(now, now.Add(-time.Hour), 60); code(err) != codes.InvalidArgument {
		t.Errorf("end before start: %v", err)
	}
	for _, q := range []string{"", "  ", strings.Repeat("x", maxMetricsQuery+1)} {
		if _, err := s.QueryMetrics(ctx, &agentfleetv1.CoreQueryMetricsRequest{SessionId: id, Query: q}); code(err) != codes.InvalidArgument {
			t.Errorf("query %.10q: %v", q, err)
		}
	}
	s2, id2, _ := reachCharacter(t, `{"sidecarTools":["send_message"]}`)
	s2.SetMetrics(&fakeMetrics{})
	if _, err := s2.QueryMetrics(ctx, &agentfleetv1.CoreQueryMetricsRequest{SessionId: id2, Query: "up"}); code(err) != codes.PermissionDenied {
		t.Errorf("no grant: %v", err)
	}
}
