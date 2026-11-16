//go:build integration

package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

type fakeOpener struct{ repo, title, body string }

func (f *fakeOpener) OpenSupervised(_ context.Context, repo, title, body string) (string, error) {
	f.repo, f.title, f.body = repo, title, body
	return "new-session", nil
}

// docs/adr/0086 §4 over real Postgres: each core-run verb runs exactly its bound payload once the owner
// chooses it, an approval whose payload no longer hashes is refused with the card still open, and
// executor_step cannot be filed.
func TestAnswerDecision_CoreVerbs(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	decs := decisions.NewStore(pool)
	op := &fakeOpener{}
	var remRaw, prReq = "", (*agentfleetv1.OpenPullRequestRequest)(nil)
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1"}}, dash: dash, sessions: store, transcr: transcr,
		hub: dashboard.NewHub(), decisions: decs, opener: op,
		cardFx: CardEffects{
			Remediate: func(_ context.Context, responder string, raw []byte, _ remediations.Bounds) (string, error) {
				remRaw = responder + " " + string(raw)
				return "rem1", nil
			},
			OpenPR: func(_ context.Context, r *agentfleetv1.OpenPullRequestRequest) (*agentfleetv1.OpenPullRequestResponse, error) {
				prReq = r
				return &agentfleetv1.OpenPullRequestResponse{Url: "u"}, nil
			},
		}}
	path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(asSub{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)

	n := 0
	draft := func(verb, payload string) string {
		n++
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1",
			Owner: "client:bridge", Visibility: "private", Invoker: "bridge", ThreadKey: "alert/" + strings.Repeat(string(rune('a'+n)), 16)})
		if err != nil {
			t.Fatal(err)
		}
		r, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: id, From: "system", Ask: "Run it?",
			Subject: &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: strings.Repeat(string(rune('a'+n)), 16)},
			Options: []*agentfleetv1.CardOption{
				{Label: "Do it", Reversible: "partial", Effect: &agentfleetv1.CardEffect{Verb: verb, Payload: payload}},
				{Label: "No", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
			}})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	answer := func(id string) error {
		_, err := c.AnswerDecision(ctx, as("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: id, OptionId: "1"}))
		return err
	}
	state := func(id string) string { r, _ := decs.Get(ctx, id); return r.State }

	// catalogue: core runs the bound selection for the writer's definition.
	id := draft("catalogue", `{"action":"restart","kind":"deployment","namespace":"game","name":"web"}`)
	if err := answer(id); err != nil || state(id) != decisions.Answered ||
		remRaw != `d {"action":"restart","kind":"deployment","namespace":"game","name":"web"}` {
		t.Fatalf("catalogue: %v %s %q", err, state(id), remRaw)
	}

	// open_session: a supervised session, the brief an agent-authored entry with the title labelled.
	id = draft("open_session", `{"repo":"notes","title":"Look at web","findings":[{"name":"restarts","value":4}]}`)
	if err := answer(id); err != nil || op.repo != "notes" || op.title != "Look at web" ||
		!strings.Contains(op.body, "title (agent claim): Look at web") || !strings.Contains(op.body, "restarts: 4") {
		t.Fatalf("open_session: %v %+v", err, op)
	}

	// open_pr: the approved commit goes to the provisioner, which checks the branch is still there.
	sha := strings.Repeat("a", 40)
	id = draft("open_pr", `{"repo":"o/r","branch":"fix/x","headSha":"`+sha+`","title":"Fix x"}`)
	if err := answer(id); err != nil || prReq.GetRepo() != "o/r" || prReq.GetBranch() != "fix/x" || prReq.GetHeadSha() != sha {
		t.Fatalf("open_pr: %v %v", err, prReq)
	}

	// The payload changed behind the owner's back: refused, keyed stale, nothing ran, card still open.
	remRaw = ""
	id = draft("catalogue", `{"action":"restart","kind":"deployment","namespace":"game","name":"web"}`)
	if _, err := pool.Exec(ctx, `UPDATE decisions SET payload = replace(payload::text, 'web', 'db')::jsonb WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := answer(id); errkeys.Of(err) != errkeys.KeyDecisionStale || remRaw != "" || state(id) != decisions.Open {
		t.Errorf("tampered payload: %v key %q ran %q state %s", err, errkeys.Of(err), remRaw, state(id))
	}

	// A refusal by the service after the answer stands is reported, not swallowed.
	s.cardFx.Remediate = func(context.Context, string, []byte, remediations.Bounds) (string, error) {
		return "", remediations.ErrRateLimited
	}
	id = draft("catalogue", `{"action":"restart","kind":"deployment","namespace":"game","name":"api"}`)
	if err := answer(id); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("a refused selection: %v", err)
	}

	// A surface's page can click whatever it draws: a core verb is never answered in place, even reversible "yes".
	remRaw = ""
	n++
	sw, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1",
		Owner: "client:bridge", Visibility: "private", Invoker: "bridge", ThreadKey: "alert/zzzzzzzzzzzzzzzz"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: sw, From: "system", Ask: "Run it?",
		Subject: &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: "0123456789abcdef"},
		Options: []*agentfleetv1.CardOption{
			{Label: "Do it", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "catalogue", Payload: `{"action":"restart","kind":"deployment","namespace":"game","name":"web"}`}},
			{Label: "No", Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}},
		}}); err != nil {
		t.Fatal(err)
	}
	var surfaceID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM decisions WHERE session_id = $1`, sw).Scan(&surfaceID); err != nil {
		t.Fatal(err)
	}
	sreq := as("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: surfaceID, OptionId: "1"})
	sreq.Header().Set("X-Origin", "https://game.bnei.dev")
	sreq.Header().Set("X-Surface", "1")
	if _, err := c.AnswerDecision(ctx, sreq); errkeys.Of(err) != errkeys.KeyDecisionConfirmRequired || remRaw != "" || state(surfaceID) != decisions.Open {
		t.Errorf("a surface answering a core verb: %v ran %q", err, remRaw)
	}

	// executor_step is refused until ADR-0072's executor exists, at filing and at running.
	if err := (&Server{}).checkEffect(ctx, decisions.Record{}, &agentfleetv1.CardEffect{Verb: decisions.VerbExecutorStep}); err == nil || !strings.Contains(err.Error(), "ADR-0072") {
		t.Errorf("executor_step at run: %v", err)
	}
}
