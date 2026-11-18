//go:build integration

package agentsvc

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0084 amendment A6: a decision carries the character and thread core reads from the session row, for the
// character's own session (the thread of the entry that holds it), for a run it spawned (by its principal), and not
// for any other session.
func TestDecisionCharacterJoin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	decs := decisions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('ada', 'sha1', '{}', '{}', true), ('runner', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	ada, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "ada", DefinitionSHA: "sha1",
		Owner: "client:standing:ada", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:ada", ThreadKey: "main", StartedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "runner", DefinitionSHA: "sha1",
		Owner: "client:standing:ada", Visibility: "private", Lifecycle: "single-shot", Invoker: "standing:ada", ThreadKey: "spawn-1"})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "runner", DefinitionSHA: "sha1",
		Owner: "client:bridge", Visibility: "private", Lifecycle: "single-shot", Invoker: "bridge", ThreadKey: "github/o/r/pr/9"})
	if err != nil {
		t.Fatal(err)
	}
	card := func(session, thread string, n int64) decisions.Record {
		t.Helper()
		r, err := decs.Draft(transcript.WithThread(ctx, thread), transcr, decisions.Draft{SessionID: session, From: "agent", Ask: "Merge?",
			Subject: &agentfleetv1.CardSubject{Kind: "pr", Repo: "o/r", Number: n},
			Options: []*agentfleetv1.CardOption{
				{Label: "Yes", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
				{Label: "No", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
			}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := decs.Get(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		what              string
		r                 decisions.Record
		character, thread string
	}{
		{"the character's main thread", card(ada, "main", 1), "ada", "main"},
		{"the character's chat thread", card(ada, "chat", 2), "ada", "chat"},
		{"a run it spawned", card(run, "main", 3), "ada", "spawn-1"},
		{"any other session", card(plain, "main", 4), "", ""},
	} {
		d := decisionToProto(tc.r)
		if d.GetCharacter() != tc.character || d.GetThreadKey() != tc.thread {
			t.Errorf("%s: character %q thread %q, want %q %q", tc.what, d.GetCharacter(), d.GetThreadKey(), tc.character, tc.thread)
		}
	}

	// sessions.thread_key is nullable: a standing row without one and a request entry that is gone must read as an
	// empty thread, not fail the whole listing.
	orphan := card(ada, "main", 5)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET thread_key = NULL WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE decisions SET request_seq = 99999 WHERE id = $1`, orphan.ID); err != nil {
		t.Fatal(err)
	}
	got, err := decs.Get(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("a standing row with no thread_key: %v", err)
	}
	if d := decisionToProto(got); d.GetCharacter() != "ada" || d.GetThreadKey() != "" {
		t.Errorf("orphan: character %q thread %q, want ada and none", d.GetCharacter(), d.GetThreadKey())
	}
}
