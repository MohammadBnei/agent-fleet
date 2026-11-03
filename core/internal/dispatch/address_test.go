package dispatch

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

func reviewer(name, repo string) definitions.Head {
	return definitions.Head{Name: name, SHA: "sha", Effective: &definitions.Spec{Credentials: &definitions.Credentials{
		Vault: "v", Services: []definitions.Service{{Host: "github.com", Paths: []string{"/" + repo + ".git/*"}, Secret: "GH_X"}},
	}}}
}

// Slice 5: @agent-reviewer addresses agent-reviewer-<x> on its own repo
// only, case-insensitive; the exact name still addresses it.
func TestReviewerAlias(t *testing.T) {
	comment := func(subject, body string) *bridgev1.CloudEvent {
		return &bridgev1.CloudEvent{Type: "dev.bnei.github.issue_comment.created", Subject: subject,
			Data: []byte(`{"comment":{"body":"` + body + `"}}`)}
	}
	own := reviewer("agent-reviewer-agent-fleet", "MohammadBnei/agent-fleet")
	for _, c := range []struct {
		name string
		h    definitions.Head
		e    *bridgev1.CloudEvent
		want bool
	}{
		{"the alias on its repo", own, comment("github/MohammadBnei/agent-fleet/pr/3", "@agent-reviewer please"), true},
		{"the alias, repo case differs", own, comment("github/mohammadbnei/Agent-Fleet/pr/3", "@agent-reviewer"), true},
		{"the exact name", own, comment("github/MohammadBnei/agent-fleet/pr/3", "@agent-reviewer-agent-fleet"), true},
		{"the alias on another repo", own, comment("github/MohammadBnei/notes/pr/3", "@agent-reviewer"), false},
		{"the alias on a same-named repo of another owner", own, comment("github/evil/agent-fleet/pr/3", "@agent-reviewer"), false},
		{"the alias for a name without the prefix", reviewer("notes-reviewer", "MohammadBnei/notes"), comment("github/MohammadBnei/notes/pr/3", "@agent-reviewer"), false},
		{"the alias for a definition without a GitHub credential", definitions.Head{Name: "agent-reviewer-x", Effective: &definitions.Spec{}},
			comment("github/MohammadBnei/agent-fleet/pr/3", "@agent-reviewer"), false},
		{"the alias in code", own, comment("github/MohammadBnei/agent-fleet/pr/3", "`@agent-reviewer`"), false},
	} {
		if got := addresses(c.e, names(c.h, c.e.GetSubject())); got != c.want {
			t.Errorf("%s: addresses %v, want %v", c.name, got, c.want)
		}
	}
	label := &bridgev1.CloudEvent{Type: "dev.bnei.github.pull_request.labeled", Subject: "github/MohammadBnei/agent-fleet/pr/3", ActorLogin: "o",
		Data: []byte(`{"label":{"name":"agent-reviewer"},"sender":{"login":"o"}}`)}
	if !addresses(label, names(own, label.GetSubject())) {
		t.Error("the alias label does not address the reviewer")
	}
}

// Slice 5: a hand-off wakes only who its text @mentions, never its publisher.
func TestHandOffTargets(t *testing.T) {
	hop := func(from, text string) *bridgev1.CloudEvent {
		return &bridgev1.CloudEvent{Type: "dev.bnei.agent.handoff", Subject: "github/MohammadBnei/agent-fleet/pr/3",
			Actor: "agent", ActorLogin: "agent:" + from, Data: []byte(`{"text":"` + text + `","definition":"` + from + `"}`)}
	}
	writer := definitions.Head{Name: "writer", Effective: &definitions.Spec{}}
	own := reviewer("agent-reviewer-agent-fleet", "MohammadBnei/agent-fleet")
	for _, c := range []struct {
		name string
		h    definitions.Head
		e    *bridgev1.CloudEvent
		want bool
	}{
		{"mentioned", writer, hop("agent-reviewer-agent-fleet", "@writer fix the nits"), true},
		{"untargeted", writer, hop("agent-reviewer-agent-fleet", "review done"), false},
		{"someone else mentioned", writer, hop("agent-reviewer-agent-fleet", "@other fix it"), false},
		{"quoted", writer, hop("agent-reviewer-agent-fleet", "> @writer"), false},
		{"its own publisher", writer, hop("writer", "@writer note to self"), false},
		{"the alias", own, hop("writer", "@agent-reviewer look again"), true},
		{"the alias, from itself", own, hop("agent-reviewer-agent-fleet", "@agent-reviewer again"), false},
		{"no text", writer, &bridgev1.CloudEvent{ActorLogin: "agent:x", Data: []byte(`{"ask":"@writer"}`)}, false},
	} {
		if got := handsOff(c.e, c.h); got != c.want {
			t.Errorf("%s: handsOff %v, want %v", c.name, got, c.want)
		}
	}
}
