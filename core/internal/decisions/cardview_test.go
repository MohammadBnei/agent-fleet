package decisions

import (
	"errors"
	"strings"
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// shape is a card view's options as id/verb/reversible, for comparing.
func shape(c *agentfleetv1.Card) string {
	var s []string
	for _, o := range c.GetOptions() {
		s = append(s, o.GetId()+"/"+o.GetEffect().GetVerb()+"/"+o.GetReversible())
	}
	return strings.Join(s, " ")
}

func TestCardView_Question(t *testing.T) {
	one := `{"questions":[{"question":"Which store?","options":[{"label":"pg","description":"durable"},{"label":"redis","description":"fast"}]}]}`
	c := CardView(Record{Kind: Question, Payload: one})
	if c.GetAsk() != "Which store?" || !c.GetWriteOwn() || shape(c) != "q1/choose/yes q2/choose/yes" || c.GetOptions()[1].GetLabel() != "redis" {
		t.Fatalf("one question: %v", c)
	}
	if c.GetDetail() != "Which store?\n- pg: durable\n- redis: fast" {
		t.Errorf("detail = %q", c.GetDetail())
	}
	// Anything but one single-select question of 2-3 options: own words only.
	for name, p := range map[string]string{
		"multiSelect":  `{"questions":[{"question":"q","multiSelect":true,"options":[{"label":"a"},{"label":"b"}]}]}`,
		"four options": `{"questions":[{"question":"q","options":[{"label":"a"},{"label":"b"},{"label":"c"},{"label":"d"}]}]}`,
		"one option":   `{"questions":[{"question":"q","options":[{"label":"a"}]}]}`,
	} {
		if c := CardView(Record{Kind: Question, Payload: p}); c == nil || len(c.GetOptions()) != 0 || !c.GetWriteOwn() {
			t.Errorf("%s: %v", name, c)
		}
	}
	// Several questions: no option and no own words (one reply cannot answer each by name): the console answers.
	if c := CardView(Record{Kind: Question, Payload: `{"questions":[{"question":"q","options":[{"label":"a"},{"label":"b"}]},{"question":"r"}]}`}); c == nil || len(c.GetOptions()) != 0 || c.GetWriteOwn() || !strings.Contains(c.GetDetail(), "r") {
		t.Errorf("two questions: %v", c)
	}
	if c := CardView(Record{Kind: Question, Payload: `not json`}); c != nil {
		t.Errorf("no question: %v", c)
	}
}

func TestCardView_Permission(t *testing.T) {
	payload := `{"tool":"Bash","input":{"timeout":5, "command":"ls"},"nb":1}`
	for _, c := range []struct {
		name                   string
		nonBlocking, grantable bool
		want, allowLabel       string
	}{
		{"stamped grantable", true, true, "allow/grant/no deny/deny/yes", "Allow once"},
		{"stamped ungrantable", true, false, "deny/deny/yes", ""},
		{"unstamped", false, false, "allow/grant/no deny/deny/yes", "Allow (while its pod waits)"},
	} {
		v := CardView(Record{Kind: Permission, Payload: payload, NonBlocking: c.nonBlocking, Grantable: c.grantable})
		if shape(v) != c.want || v.GetWriteOwn() || v.GetAsk() != "Allow Bash?" {
			t.Errorf("%s: %v", c.name, v)
		}
		if c.allowLabel != "" && v.GetOptions()[0].GetLabel() != c.allowLabel {
			t.Errorf("%s: allow label %q", c.name, v.GetOptions()[0].GetLabel())
		}
		if v.GetDetail() != `{"command":"ls","timeout":5}` {
			t.Errorf("%s: detail %q", c.name, v.GetDetail())
		}
	}
	if c := CardView(Record{Kind: Permission, Payload: `{}`}); c != nil {
		t.Errorf("no tool: %v", c)
	}
}

func TestCardView_PlanAndCard(t *testing.T) {
	long := strings.Repeat("step. ", 100)
	c := CardView(Record{Kind: Plan, Payload: `{"tool":"ExitPlanMode","input":{"plan":"` + long + `"}}`})
	if shape(c) != "approve/approve_plan/partial keep_planning/keep_planning/yes" || c.GetWriteOwn() ||
		c.GetDetail() != long || len([]rune(c.GetAsk())) != maxAsk {
		t.Errorf("plan: %v", c)
	}
	if c := CardView(Record{Kind: Plan, Payload: `{"tool":"ExitPlanMode"}`}); c.GetAsk() != "Approve the plan?" {
		t.Errorf("empty plan ask %q", c.GetAsk())
	}
	stored := `{"ask":"Merge?","writeOwn":true,"options":[{"id":"1","label":"Yes","effect":{"verb":"handoff"},"reversible":"partial"}]}`
	if c := CardView(Record{Kind: Card, Payload: stored}); c.GetAsk() != "Merge?" || !c.GetWriteOwn() || shape(c) != "1/handoff/partial" {
		t.Errorf("card: %v", c)
	}
	if c := CardView(Record{Kind: "other"}); c != nil {
		t.Errorf("unknown kind: %v", c)
	}
}

func TestAnswerOption(t *testing.T) {
	question := Record{Kind: Question, Payload: `{"questions":[{"question":"Which store?","options":[{"label":"pg"},{"label":"redis"}]}]}`}
	perm := Record{Kind: Permission, Payload: `{"tool":"Bash","input":{},"nb":1}`, NonBlocking: true, Grantable: true}
	ungrantable := perm
	ungrantable.Grantable = false
	plan := Record{Kind: Plan, Payload: `{"tool":"ExitPlanMode","input":{"plan":"p"}}`}
	card := Record{Kind: Card, Payload: `{"ask":"a","options":[{"id":"1"}]}`}
	for _, c := range []struct {
		name          string
		r             Record
		option, words string
		want          Reply
	}{
		{"question option", question, "q2", "", Reply{Answers: `{"answers":{"Which store?":"redis"}}`}},
		{"question own words", question, "", "sqlite", Reply{Answers: `{"answers":{"Which store?":"sqlite"}}`}},
		{"allow", perm, "allow", "", Reply{Behavior: "allow"}},
		{"deny", perm, "deny", "", Reply{Behavior: "deny"}},
		{"approve", plan, "approve", "", Reply{Behavior: "allow"}},
		{"keep planning", plan, "keep_planning", "", Reply{Behavior: "deny", Message: "Keep planning."}},
		{"card", card, "1", "", Reply{OptionID: "1"}},
	} {
		got, err := AnswerOption(c.r, c.option, c.words)
		if err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		name          string
		r             Record
		option, words string
	}{
		{"neither", question, "", ""},
		{"both", question, "q1", "x"},
		{"no such option", question, "q3", ""},
		{"own words on a permission", perm, "", "sure"},
		{"allow an ungrantable stamped request", ungrantable, "allow", ""},
		{"no view", Record{Kind: Permission, Payload: `{}`}, "deny", ""},
	} {
		if _, err := AnswerOption(c.r, c.option, c.words); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", c.name, err)
		}
	}
}
