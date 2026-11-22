package dispatch

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/events"
	"github.com/MohammadBnei/agent-fleet/core/internal/forge"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

// realLabels is a bridge label_updated fixture as Forgejo sends it: its "label" key is the fixture's own, which
// Forgejo's IssueChangeLabels notifier never sets; the PR's or issue's labels after the change carry it instead.
func realLabels(t *testing.T, data []byte) []byte {
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	l := p["label"]
	delete(p, "label")
	for _, k := range []string{"pull_request", "issue"} {
		if on, ok := p[k].(map[string]any); ok {
			on["labels"] = []any{l}
		}
	}
	out, _ := json.Marshal(p)
	return out
}

// The bridge's Forgejo deliveries as core receives them: testdata/forgejo is ukubi-bridge's
// internal/forgejo/testdata, and each row's envelope (type, subject, actor, actor_trusted, thread_owned) is what its
// TestNormalize asserts for that fixture.
func TestForgejoEvents(t *testing.T) {
	const home = "forge/MohammadBnei/editable-blog/"
	for _, c := range []struct {
		fixture, typ, subject   string
		actor, login            string
		trusted, owned, dropped bool
		pr                      int64
		name                    string // addressed when addr
		addr                    bool
		render                  string // in Render's message
	}{
		{"pull_request_opened", "pull_request.opened", "pr/42", "agent", "ukubi-fleet", false, true, true, 42, "ukubi-fleet", false, "> Closes #9"},
		// The fleet's close is not dropped: it closes the thread.
		{"pull_request_closed", "pull_request.closed", "pr/42", "human", "stranger", false, true, false, 42, "", false, "url: https://git.bnei.dev/MohammadBnei/editable-blog/pulls/42"},
		// The label is the sender's: it addresses the definition it names.
		{"pull_request_label_updated", "pull_request.label_updated", "pr/42", "human", "MohammadBnei", true, false, false, 42, "dev-notes", true, "labels: dev-notes"},
		{"issues_label_updated", "issues.label_updated", "issue/9", "human", "MohammadBnei", true, false, false, 0, "dev-notes", true, "labels: dev-notes"},
		{"issue_comment_created", "issue_comment.created", "issue/9", "human", "stranger", false, true, false, 0, "ukubi-fleet", true, "> @ukubi-fleet please take this"},
		{"pull_request_comment_created", "issue_comment.created", "pr/42", "human", "MohammadBnei", true, true, false, 42, "ukubi-fleet", true, "> @ukubi-fleet please take this"},
		{"pull_request_comment_fork", "issue_comment.created", "pr/42", "human", "MohammadBnei", true, false, false, 42, "ukubi-fleet", true, "> @ukubi-fleet please take this"},
		// A review's text is review.content; its state is the type's action.
		{"pull_request_review_approved", "pull_request_review.approved", "pr/42", "agent", "ukubi-review", false, true, true, 42, "", false, "state: approved\n"},
		{"pull_request_review_rejected", "pull_request_review.rejected", "pr/42", "human", "MohammadBnei", true, true, false, 42, "", false, "> Needs a test."},
		// CI's verdict on the bot's push is not the bot's own action: not dropped.
		{"action_run_failure", "action_run.failure", "run/117", "agent", "ukubi-fleet", false, true, false, 0, "", false, "status: failure"},
	} {
		data, err := os.ReadFile("testdata/forgejo/" + c.fixture + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(c.typ, ".label_updated") {
			data = realLabels(t, data)
		}
		e := &bridgev1.CloudEvent{Id: "8a0f7c1e", Source: "git.bnei.dev/MohammadBnei/editable-blog", Type: "dev.bnei.forge." + c.typ,
			Subject: home + c.subject, Actor: c.actor, ActorLogin: c.login, ActorTrusted: c.trusted, ThreadOwned: c.owned, Data: data}
		if !events.TypeMatches(Types, e.GetType()) {
			t.Errorf("%s: core does not subscribe to %s", c.fixture, e.GetType())
		}
		if (e.GetType() == typePRClosed) != (c.fixture == "pull_request_closed") {
			t.Errorf("%s: closes a thread: %v", c.fixture, e.GetType() == typePRClosed)
		}
		if full, name := forge.RepoOf(e.GetSubject()); full != "MohammadBnei/editable-blog" || name != "editable-blog" {
			t.Errorf("%s: repo %q %q", c.fixture, full, name)
		}
		if repo, n, ok := forge.PROf(e.GetSubject()); ok != (c.pr > 0) || n != c.pr || (ok && repo != "MohammadBnei/editable-blog") {
			t.Errorf("%s: PR %q #%d %v, want #%d", c.fixture, repo, n, ok, c.pr)
		}
		if botEvent(e) != (c.actor == "agent") || selfEcho(e) != c.dropped {
			t.Errorf("%s: botEvent %v, dropped %v", c.fixture, botEvent(e), selfEcho(e))
		}
		if c.name != "" && addresses(e, []string{c.name}) != c.addr {
			t.Errorf("%s: addresses @%s %v, want %v", c.fixture, c.name, !c.addr, c.addr)
		}
		if addresses(e, []string{"nobody"}) {
			t.Errorf("%s: addresses a name it does not carry", c.fixture)
		}
		if m := Render(e, false); !strings.Contains(m, c.render) {
			t.Errorf("%s: Render lacks %q:\n%s", c.fixture, c.render, m)
		}
		// Waking on a thread that is not ours: the subject's title is its author's, never in the message.
		if !c.owned {
			if m := Render(e, true); strings.Contains(m, "Dark mode") || strings.Contains(m, "feat: dark mode") || !strings.Contains(m, c.render) {
				t.Errorf("%s: a wake on an unowned thread:\n%s", c.fixture, m)
			}
		}
	}
}

// A label_updated whose labels no longer hold the definition's is its removal: it addresses nobody.
func TestForgejoLabelRemoved(t *testing.T) {
	data, err := os.ReadFile("testdata/forgejo/pull_request_label_updated.json")
	if err != nil {
		t.Fatal(err)
	}
	e := &bridgev1.CloudEvent{Type: forge.Type + "pull_request.label_updated", ActorLogin: "MohammadBnei", Data: data}
	if addresses(e, []string{"dev-notes"}) {
		t.Error("a label_updated without the label in pull_request.labels addresses it (the fixture's own \"label\" is not Forgejo's)")
	}
}

// A closed PR's payload: merged and base.ref against repository.default_branch.
func TestForgejoMergedIntoDefault(t *testing.T) {
	data, err := os.ReadFile("testdata/forgejo/pull_request_closed.json")
	if err != nil {
		t.Fatal(err)
	}
	if mergedIntoDefault(&bridgev1.CloudEvent{Data: data}) {
		t.Error("an unmerged close reads merged")
	}
	merged := strings.Replace(string(data), `"merged": false`, `"merged": true`, 1)
	if merged == string(data) || !mergedIntoDefault(&bridgev1.CloudEvent{Data: []byte(merged)}) {
		t.Error("a merge into main reads unmerged")
	}
}
