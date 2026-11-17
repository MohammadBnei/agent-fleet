//go:build integration

package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
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
	if _, err := start(ctx, "dev-dream-analyst"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Start of a definition that is not standing: %v", err)
	}
	if _, err := start(ctx, "nope"); code(err) != connect.CodeNotFound {
		t.Fatalf("Start of an unknown definition: %v", err)
	}
	if err := stop("dev-stander"); code(err) != connect.CodeNotFound {
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
	if _, err := start(ctx, "dev-stander"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Start of a running character: %v", err)
	}

	// Stop: marked, an abort entry for a live pod, and no warm until Start.
	if err := stop("dev-stander"); err != nil {
		t.Fatal(err)
	}
	if err := stop("dev-stander"); code(err) != connect.CodeFailedPrecondition {
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
	if _, err := start(ctx, "dev-stander"); code(err) != connect.CodeAborted {
		t.Fatalf("Start over a live pod: %v", err)
	}
	var armed bool
	if err := pool.QueryRow(ctx, `SELECT stopped_at IS NOT NULL AND stop_requested_at IS NOT NULL FROM sessions WHERE id = $1`, row.GetId()).Scan(&armed); err != nil || !armed {
		t.Fatalf("a refused Start disarmed the stop: %v %v", armed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = NULL WHERE id = $1`, row.GetId()); err != nil {
		t.Fatal(err)
	}

	// Start resumes the same row with a fresh SDK session, and the transcript stays.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET agent_session_id = 'old-sdk', definition_sha = definition_sha WHERE id = $1`, row.GetId()); err != nil {
		t.Fatal(err)
	}
	again, err := start(ctx, "dev-stander")
	if err != nil || again.GetId() != row.GetId() {
		t.Fatalf("Start of a stopped character: %v %v", again.GetId(), err)
	}
	if got, _ = store.Get(ctx, row.GetId()); got.StoppedAt != nil || got.AgentSessionID != nil {
		t.Fatalf("resumed row: stopped %v sdk session %v", got.StoppedAt, got.AgentSessionID)
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
