package responder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/remediations"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// ErrOutput: the responder's structured output is not one of the union's three shapes.
var ErrOutput = errors.New("responder output is invalid")

const (
	OutcomeCatalogue = "catalogue"
	OutcomeCard      = "card"
	OutcomeNothing   = "nothing"
)

// Output is the typed union (docs/adr/0086 §3): a catalogue selection, a card request, or nothing to do. Nothing in
// it is free text: a card's findings are enums, numbers and object ids, which core checks and renders itself.
type Output struct {
	Outcome   string          `json:"outcome"`
	Selection json.RawMessage `json:"selection,omitempty"`
	Card      *CardRequest    `json:"card,omitempty"`
}

type CardRequest struct {
	Kind     string    `json:"kind"`
	Objects  []Object  `json:"objects,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
}

type Object struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type Finding struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// cardKinds is the fixed enum, with the words the owner reads. A rollback is a revert PR (ADR-0086 §3).
var cardKinds = map[string]string{
	"code_fix":        "needs a code fix",
	"revert_pr":       "needs a revert",
	"ansible_step":    "needs an Ansible step",
	"unknown_failure": "failed in a way I could not name",
}

var (
	objectKinds  = []string{"deployment", "statefulset", "daemonset", "pod", "application", "node", "namespace"}
	findingNames = []string{"restarts", "replicas_ready", "replicas_desired", "cpu_pct", "memory_pct", "error_rate_pct", "node_count"}
	dns1123      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

const maxList = 8

// Parse decodes and checks one run's structured output, refusing unknown fields, trailing data, and any shape
// that mixes the union's arms.
func Parse(raw string) (Output, error) {
	var o Output
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil || dec.More() {
		return Output{}, fmt.Errorf("%w: unreadable", ErrOutput)
	}
	bad := func(f string, a ...any) (Output, error) {
		return Output{}, fmt.Errorf("%w: %s", ErrOutput, fmt.Sprintf(f, a...))
	}
	switch o.Outcome {
	case OutcomeNothing:
		if len(o.Selection) > 0 || o.Card != nil {
			return bad("nothing carries no payload")
		}
	case OutcomeCatalogue:
		if len(o.Selection) == 0 || o.Card != nil {
			return bad("catalogue carries a selection alone")
		}
	case OutcomeCard:
		c := o.Card
		if len(o.Selection) > 0 || c == nil {
			return bad("card carries a card alone")
		}
		if _, ok := cardKinds[c.Kind]; !ok {
			return bad("card.kind %q", c.Kind)
		}
		if len(c.Objects) > maxList || len(c.Findings) > maxList {
			return bad("at most %d objects and %d findings", maxList, maxList)
		}
		for _, ob := range c.Objects {
			if !contains(objectKinds, ob.Kind) || !dns1123.MatchString(ob.Name) || len(ob.Name) > 253 ||
				(ob.Namespace != "" && (!dns1123.MatchString(ob.Namespace) || len(ob.Namespace) > 63)) {
				return bad("object %q", ob.Kind)
			}
		}
		for _, f := range c.Findings {
			if !contains(findingNames, f.Name) || f.Value < 0 || f.Value > 1e9 {
				return bad("finding %q", f.Name)
			}
		}
	default:
		return bad("outcome %q", o.Outcome)
	}
	return o, nil
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// Ask is the card's text, written by core from the checked request and the alert name core resolved.
func (c CardRequest) Ask(g Group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Alert %s %s.", g.Name, cardKinds[c.Kind])
	for _, f := range c.Findings {
		b.WriteString(" " + f.Name + "=" + strconv.FormatFloat(f.Value, 'f', -1, 64))
	}
	for _, o := range c.Objects {
		b.WriteString(" " + o.Kind + "/")
		if o.Namespace != "" {
			b.WriteString(o.Namespace + "/")
		}
		b.WriteString(o.Name)
	}
	return clip(b.String(), 280)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Filer files a run's output. Select is remediations.Service.Select; Draft is decisions.Store.Draft into the run's
// thread.
type Filer struct {
	Select func(ctx context.Context, responder string, raw []byte, b remediations.Bounds) (string, error)
	Draft  func(ctx context.Context, d decisions.Draft) error
}

// File turns the structured output of thread's run into a remediations row or a card. A selection core refuses
// (denylist, rate limit, not low-stakes) becomes a card, so the owner still hears of the alert.
func (f Filer) File(ctx context.Context, def string, spec *definitions.Spec, thread string, g Group, structured string) error {
	out, err := Parse(structured)
	if err != nil {
		return err
	}
	switch out.Outcome {
	case OutcomeNothing:
		return nil
	case OutcomeCard:
		return f.card(ctx, thread, g, out.Card.Ask(g))
	}
	b := remediations.Bounds{}
	if spec.Trust != nil && spec.Trust.Scale != nil {
		b = remediations.Bounds{Min: spec.Trust.Scale.Min, Max: spec.Trust.Scale.Max}
	}
	id, err := f.Select(ctx, def, out.Selection, b)
	if err == nil {
		slog.Info("responder: filed a remediation", "definition", def, "group", g.Key, "remediation", id)
		return nil
	}
	for _, refused := range []error{remediations.ErrInvalid, remediations.ErrDenied, remediations.ErrNotLowStakes, remediations.ErrRateLimited} {
		if errors.Is(err, refused) {
			slog.Warn("responder: selection refused, filing a card", "definition", def, "group", g.Key, "reason", refused.Error())
			return f.card(ctx, thread, g, clip("Alert "+g.Name+": the responder's fix was refused: "+refused.Error()+".", 280))
		}
	}
	return err
}

// Unanswered is the card for a run that ended without a usable answer, written by core.
func (f Filer) Unanswered(ctx context.Context, thread string, g Group) error {
	return f.card(ctx, thread, g, clip("Alert "+g.Name+": the responder could not answer. Look at it yourself.", 280))
}

func (f Filer) card(ctx context.Context, thread string, g Group, ask string) error {
	handoff := func(label string) *agentfleetv1.CardOption {
		return &agentfleetv1.CardOption{Label: label, Reversible: "yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}}
	}
	return f.Draft(ctx, decisions.Draft{
		SessionID: thread, From: "system", IdempotencyKey: "responder-card:" + thread, Ask: ask,
		Subject: &agentfleetv1.CardSubject{Kind: "alert_group", GroupKey: g.Key},
		Options: []*agentfleetv1.CardOption{handoff("I will handle it"), handoff("Dismiss")},
	})
}
