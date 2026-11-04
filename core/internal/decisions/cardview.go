package decisions

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// cardView is one kind's card view (docs/adr/0082 §5): the card core builds
// from its stored request, and the Reply an option or the owner's own words
// on it make for that kind's existing answer path. reply is called only with
// an option the view has, or own words on a write_own view.
type cardView struct {
	card  func(Record) *agentfleetv1.Card
	reply func(r Record, c *agentfleetv1.Card, optionID, ownText string) Reply
}

var cardViews = map[Kind]cardView{
	Question:   {questionCard, questionReply},
	Permission: {permissionCard, behaviorReply},
	Plan:       {planCard, behaviorReply},
	// A card is its own view, answered as it always was (answerCard).
	Card: {storedCard, func(_ Record, _ *agentfleetv1.Card, optionID, ownText string) Reply {
		return Reply{OptionID: optionID, OwnText: ownText}
	}},
}

// CardView is r as a card: a card's own, any other kind's built from its
// request. Nil when none can be built (an unknown kind, or a request with
// nothing to ask).
func CardView(r Record) *agentfleetv1.Card {
	v, ok := cardViews[r.Kind]
	if !ok {
		return nil
	}
	return v.card(r)
}

// AnswerOption is the Reply exactly one of an option of r's card view or
// the owner's own words (a write_own view only) makes, for Answer. The
// caller sets By.
func AnswerOption(r Record, optionID, ownText string) (Reply, error) {
	if (optionID == "") == (ownText == "") {
		return Reply{}, fmt.Errorf("%w: a decision is answered with one option or the owner's own words", ErrInvalid)
	}
	c := CardView(r)
	switch {
	case c == nil:
		return Reply{}, fmt.Errorf("%w: a %s decision %s has no card view", ErrInvalid, r.Kind, r.ID)
	case ownText != "" && !c.GetWriteOwn():
		return Reply{}, fmt.Errorf("%w: this decision takes no answer of your own", ErrInvalid)
	case optionID != "" && !slices.ContainsFunc(c.GetOptions(), func(o *agentfleetv1.CardOption) bool { return o.GetId() == optionID }):
		return Reply{}, fmt.Errorf("%w: decision %s has no option %q", ErrInvalid, r.ID, optionID)
	}
	return cardViews[r.Kind].reply(r, c, optionID, ownText), nil
}

func storedCard(r Record) *agentfleetv1.Card {
	c := new(agentfleetv1.Card)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(r.Payload), c); err != nil {
		return nil
	}
	return c
}

func option(id, label, verb, reversible string) *agentfleetv1.CardOption {
	return &agentfleetv1.CardOption{Id: id, Label: clip(label, 80), Effect: &agentfleetv1.CardEffect{Verb: verb}, Reversible: reversible}
}

func view(r Record, ask, detail string, options ...*agentfleetv1.CardOption) *agentfleetv1.Card {
	return &agentfleetv1.Card{Ask: clip(ask, maxAsk), Options: options, Detail: detail, WriterDefinition: deref(r.Definition)}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// questionRequest is the AskUserQuestion payload with what a card view needs.
type questionRequest struct {
	Questions []struct {
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		MultiSelect bool `json:"multiSelect"`
	} `json:"questions"`
}

func readQuestion(r Record) questionRequest {
	var q questionRequest
	_ = json.Unmarshal([]byte(r.Payload), &q)
	return q
}

// questionCard: one question of two or three options, not multiSelect,
// gets them as options "q1".."q3"; any other (several questions, four
// options, multiSelect) gets none and is answered in the owner's own words.
// Detail is every question with its options' descriptions.
func questionCard(r Record) *agentfleetv1.Card {
	q := readQuestion(r)
	if len(q.Questions) == 0 {
		return nil
	}
	var detail []string
	for _, qq := range q.Questions {
		detail = append(detail, qq.Question)
		for _, o := range qq.Options {
			detail = append(detail, "- "+o.Label+": "+o.Description)
		}
	}
	c := view(r, q.Questions[0].Question, strings.Join(detail, "\n"))
	c.WriteOwn = true
	if first := q.Questions[0]; len(q.Questions) == 1 && !first.MultiSelect && len(first.Options) >= 2 && len(first.Options) <= 3 {
		for i, o := range first.Options {
			c.Options = append(c.Options, option(fmt.Sprintf("q%d", i+1), o.Label, "choose", "yes"))
		}
	}
	return c
}

// questionReply is the question's answers JSON (worker formatAnswerTurn):
// the chosen label in full, or the owner's words, under the first question.
func questionReply(r Record, _ *agentfleetv1.Card, optionID, ownText string) Reply {
	first := readQuestion(r).Questions[0]
	answer := ownText
	if optionID != "" {
		var i int
		_, _ = fmt.Sscanf(optionID, "q%d", &i)
		answer = first.Options[i-1].Label
	}
	b, _ := json.Marshal(map[string]map[string]string{"answers": {first.Question: answer}})
	return Reply{Answers: string(b)}
}

// permissionCard: a stamped, grantable request allows once (a one-time
// grant) or denies; a stamped one without a canonical input can only be
// denied, as no grant can match its retry; an older worker's unstamped one
// allows while its pod still waits on it. Detail is the canonical input.
func permissionCard(r Record) *agentfleetv1.Card {
	var p permission
	_ = json.Unmarshal([]byte(r.Payload), &p)
	if p.Tool == "" {
		return nil
	}
	detail, err := Canon(p.Input)
	if err != nil {
		detail = string(p.Input)
	}
	deny := option("deny", "Deny", "deny", "yes")
	switch {
	case !r.NonBlocking:
		return view(r, "Allow "+p.Tool+"?", detail, option("allow", "Allow (while its pod waits)", "grant", "no"), deny)
	case r.Grantable:
		return view(r, "Allow "+p.Tool+"?", detail, option("allow", "Allow once", "grant", "no"), deny)
	}
	return view(r, "Allow "+p.Tool+"?", detail, deny)
}

// planCard approves the plan (no mode switch, as the console's plain
// approval) or keeps planning; the mode choice stays the console's. Detail
// is the full plan.
func planCard(r Record) *agentfleetv1.Card {
	var p permission
	_ = json.Unmarshal([]byte(r.Payload), &p)
	full := plan{p}.Ask()
	ask := full
	if ask == "" {
		ask = "Approve the plan?"
	}
	return view(r, ask, full,
		option("approve", "Approve", "approve_plan", "partial"),
		option("keep_planning", "Keep planning", "keep_planning", "yes"))
}

// behaviorReply maps a permission's or a plan's option onto its allow or
// deny response.
func behaviorReply(_ Record, _ *agentfleetv1.Card, optionID, _ string) Reply {
	switch optionID {
	case "allow", "approve":
		return Reply{Behavior: "allow"}
	case "keep_planning":
		return Reply{Behavior: "deny", Message: "Keep planning."}
	}
	return Reply{Behavior: "deny"}
}
