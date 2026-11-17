//go:build integration

package agentsvc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0084 §6 over real Postgres: a character spawns only what spawns.allow lists, as T1 under its own
// principal; a retry of a key starts nothing new, the hourly cap meets new runs only, WaitForResult answers only
// the character's own runs, and a run's end appends one turn-starting entry to the character.
func TestSpawnWorker(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	eff := func(allow []string, perHour int) string {
		spawns := ""
		if allow != nil {
			spawns = `,"spawns":{"allow":["` + strings.Join(allow, `","`) + `"]},"limits":{"spawns":{"perHour":` + string(rune('0'+perHour)) + `}}`
		}
		return `{"lifecycle":{"allow":["standing"]}` + spawns + `}`
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES
		('ops', 'sha1', '{}', $1, true), ('lone', 'sha1', '{}', $2, true), ('worker', 'sha1', '{}', '{}', true), ('other', 'sha1', '{}', '{}', true), ('par', 'sha1', '{}', $3, true)`,
		eff([]string{"worker"}, 2), eff(nil, 0), eff([]string{"worker"}, 3)); err != nil {
		t.Fatal(err)
	}
	single := func() *definitions.Spec {
		return &definitions.Spec{
			Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
			Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
			Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
			Lifecycle: &definitions.AllowList{Allow: []string{"single-shot"}},
		}
	}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	s := &Server{definitions: fakeHeads{{Name: "worker", SHA: "sha1", Effective: single()}, {Name: "other", SHA: "sha1", Effective: single()}},
		dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decisions.NewStore(pool)}

	start := func(name, sha string) string {
		id, err := store.Create(ctx, sessions.CreateParams{
			Repo: "notes", PermissionMode: "default", DefinitionName: name, DefinitionSHA: sha,
			Owner: "client:standing:" + name, Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:" + name, ThreadKey: "main", StartedBy: "root",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var n0 int
	ops, lone, par := start("ops", "sha1"), start("lone", "sha1"), start("par", "sha1")
	code := connect.CodeOf

	id, err := s.SpawnWorker(ctx, ops, "worker", "do it", "k1")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	run, err := store.Get(ctx, id)
	if err != nil || run.Invoker == nil || *run.Invoker != "standing:ops" || run.Owner == nil || *run.Owner != "client:standing:ops" ||
		deref(run.Lifecycle, "") != singleShot || run.TrustSet != "BC" || deref(run.DefinitionName, "") != "worker" {
		t.Fatalf("the run is the character's, single-shot, T1: %+v %v", run, err)
	}
	if again, err := s.SpawnWorker(ctx, ops, "worker", "do it", "k1"); err != nil || again != id {
		t.Fatalf("a retry of the key: %v %v, want %s", again, err, id)
	}
	if n, _ := store.SpawnsSince(ctx, "standing:ops", 3600e9); n != 1 {
		t.Fatalf("one run counted, got %d", n)
	}
	if _, err := s.SpawnWorker(ctx, ops, "other", "x", "k2"); code(err) != connect.CodePermissionDenied {
		t.Errorf("a definition off spawns.allow: %v", err)
	}
	if _, err := s.SpawnWorker(ctx, lone, "worker", "x", "k3"); code(err) != connect.CodePermissionDenied {
		t.Errorf("a character with no spawns.allow: %v", err)
	}
	id2, err := s.SpawnWorker(ctx, ops, "worker", "second", "k4")
	if err != nil || id2 == id {
		t.Fatalf("second run: %v %v", id2, err)
	}
	// perHour 2: a third NEW run is refused, a retry of an old key is not.
	if _, err := s.SpawnWorker(ctx, ops, "worker", "third", "k5"); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "limits.spawns") {
		t.Errorf("the hourly cap: %v", err)
	}
	if again, err := s.SpawnWorker(ctx, ops, "worker", "do it", "k1"); err != nil || again != id {
		t.Errorf("a retry at the cap: %v %v", again, err)
	}

	// Spawn's input and key are bounded like Invoke's, and an empty key is named as one.
	if _, err := s.SpawnWorker(ctx, ops, "worker", strings.Repeat("x", maxText+1), "kbig"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an input over maxText: %v", err)
	}
	if _, err := s.SpawnWorker(ctx, ops, "worker", "x", strings.Repeat("k", maxKey+1)); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a key over maxKey: %v", err)
	}
	if _, err := s.SpawnWorker(ctx, ops, "worker", "x", ""); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "idempotency_key") {
		t.Errorf("an empty key: %v", err)
	}
	// Parallel spawns (parallel tool calls in one turn) cannot all pass the hourly count: perHour 3, 8 at once.
	var wg sync.WaitGroup
	var okN atomic.Int32
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SpawnWorker(ctx, par, "worker", "p", fmt.Sprintf("par%d", i)); err == nil {
				okN.Add(1)
			}
		}()
	}
	wg.Wait()
	if n, _ := store.SpawnsSince(ctx, "standing:par", 3600e9); okN.Load() != 3 || n != 3 {
		t.Errorf("perHour 3 under 8 parallel spawns: %d ok, %d counted", okN.Load(), n)
	}

	// WaitForResult answers the character's own runs only.
	if r, err := s.SpawnResult(ctx, ops, id, 0); err != nil || r.GetState() != "running" {
		t.Fatalf("own run: %v %v", r, err)
	}
	if _, err := s.SpawnResult(ctx, lone, id, 0); err == nil || code(err) != connect.CodeNotFound {
		t.Errorf("another character's run must not be seen: %v", err)
	}

	if _, err := s.SpawnResult(ctx, ops, ops, 0); err == nil || code(err) != connect.CodeNotFound {
		t.Errorf("the character's own row is no run: %v", err)
	}

	// A run's end tells the character once, with a turn-starting entry, and the run is marked.
	if _, err := transcr.Append(ctx, id, "worker", `{"subtype":"success","result_text":"42 notes tidied","consumedSeq":1,"exit":true}`, "result", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportSpawnEnds(ctx); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1 AND idempotency_key = $2`, ops, "spawn-end:"+id).Scan(&n0); err != nil || n0 != 0 {
		t.Fatalf("a run not yet archived is not reported: %d %v", n0, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportSpawnEnds(ctx); err != nil {
		t.Fatalf("report: %v", err)
	}
	var texts []string
	rows, err := pool.Query(ctx, `SELECT text FROM transcript WHERE session_id = $1 AND idempotency_key = $2`, ops, "spawn-end:"+id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		texts = append(texts, x)
	}
	rows.Close()
	if len(texts) != 1 || !strings.Contains(texts[0], id) || !strings.Contains(texts[0], "done") || !strings.Contains(texts[0], "42 notes tidied") {
		t.Fatalf("the character's entry: %q", texts)
	}
	if err := s.ReportSpawnEnds(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1 AND idempotency_key LIKE 'spawn-end:%'`, ops).Scan(&n); err != nil || n != 1 {
		t.Fatalf("told once, and the run still running is not told: %d %v", n, err)
	}
	// A stopped character takes nothing, and the run is marked all the same.
	if err := pool.QueryRow(ctx, `UPDATE sessions SET stopped_at = now() WHERE id = $1 RETURNING 1`, ops).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := transcr.Append(ctx, id2, "worker", `{"subtype":"success","result_text":"late","consumedSeq":1,"exit":true}`, "result", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportSpawnEnds(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1 AND idempotency_key = $2`, ops, "spawn-end:"+id2).Scan(&n); err != nil || n != 0 {
		t.Errorf("a stopped character was told: %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1 AND spawn_reported_at IS NOT NULL`, id2).Scan(&n); err != nil || n != 1 {
		t.Errorf("the run is marked reported anyway: %d %v", n, err)
	}
	if _, err := s.SpawnWorker(ctx, ops, "worker", "x", "k9"); code(err) != connect.CodePermissionDenied {
		t.Errorf("a stopped character spawns nothing: %v", err)
	}
}
