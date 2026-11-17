//go:build integration

package agentsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0084 §7 over real Postgres: the owner's message to a started character is a human (T0) entry on the thread
// named, main by default, and warms the character; a stopped or unknown character takes nothing. A character's
// Speak reaches a watching browser under its definition name and writes no transcript entry.
func TestPostToCharacterAndSpeak(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	s := &Server{dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub(), decisions: decisions.NewStore(pool), speech: newSpeechHub()}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('ops', 'sha1', '{}', '{}', true), ('gone', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	start := func(name string) string {
		id, err := store.Create(ctx, sessions.CreateParams{
			Repo: "r", PermissionMode: "default", DefinitionName: name, DefinitionSHA: "sha1",
			Owner: "client:standing:" + name, Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:" + name, ThreadKey: "main", StartedBy: "root",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ops, gone := start("ops"), start("gone")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET stopped_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	owner := withPrincipal(ctx, Principal{Member: &auth.Identity{Sub: "root", Email: "root@bnei.dev", Groups: []string{auth.RequiredGroup}}, Origin: "https://game.bnei.dev", Surface: true})
	post := func(character, thread string) (int64, error) {
		res, err := s.PostToCharacter(owner, connect.NewRequest(&agentfleetv1.PostToCharacterRequest{Character: character, Text: "hello", ThreadKey: thread}))
		if err != nil {
			return 0, err
		}
		return res.Msg.GetSeq(), nil
	}
	read := func(id string) []transcript.Entry {
		es, _, err := transcr.ReadSince(ctx, id, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		return es
	}

	_, err := post("ops", "")
	if err != nil {
		t.Fatalf("post to main: %v", err)
	}
	if _, err := post("ops", "chess"); err != nil {
		t.Fatalf("post to a new thread: %v", err)
	}
	var main, chess int
	for _, e := range read(ops) {
		if e.From != "human" || e.Text != "hello" || e.AuthorSub != "root" {
			continue
		}
		switch e.ThreadKey {
		case transcript.MainThread:
			main++
		case "chess":
			chess++
		}
	}
	if main != 1 || chess != 1 {
		t.Errorf("human entries: main %d, chess %d, want 1 each", main, chess)
	}
	if len(dash.warms) != 2 || dash.warms[0] != ops {
		t.Errorf("warms: %v", dash.warms)
	}
	if _, err := post("gone", ""); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a stopped character: %v", err)
	}
	if _, err := post("nobody", ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown character: %v", err)
	}
	if len(read(gone)) != 0 {
		t.Error("a stopped character took an entry")
	}

	lines, cancel := s.speech.subscribe()
	defer cancel()
	before := len(read(ops))
	if d, err := s.Speak(ctx, ops, "good morning", "happy", "eff:"+ops+":main:5:1"); err != nil || !d {
		t.Fatalf("speak: %v %v", d, err)
	}
	if l := <-lines; l.GetCharacter() != "ops" || l.GetText() != "good morning" || l.GetFace() != "happy" {
		t.Errorf("line: %+v", l)
	}
	if n := len(read(ops)); n != before {
		t.Errorf("Speak wrote %d transcript entries", n-before)
	}
}
