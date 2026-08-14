package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type mockAsker struct {
	answered    bool
	answersJSON string
	err         error
	gotPayload  string
}

func (m *mockAsker) AskUserQuestion(_ context.Context, questionsJSON string, _ int32) (bool, string, int64, error) {
	m.gotPayload = questionsJSON
	return m.answered, m.answersJSON, 1, m.err
}

func answer(label string) string {
	b, _ := json.Marshal(map[string]any{"answers": map[string]string{startCmdOverrideQuestion: label}})
	return string(b)
}

// The gate exists because an agent's guessed start command silently replaced
// a correct repo profile and left the preview returning 502 with nothing
// able to explain why. Every path that isn't an explicit human pick of the
// override must fall back to the profile — docs/adr/0029: a decision is
// never inferred from silence.
func TestConfirmStartCmdOverride(t *testing.T) {
	tests := []struct {
		name        string
		answered    bool
		answersJSON string
		err         error
		want        bool
	}{
		{name: "explicit approval", answered: true, answersJSON: answer(startCmdUseOverride), want: true},
		{name: "explicit decline", answered: true, answersJSON: answer(startCmdKeepProfile), want: false},
		{name: "timed out unanswered", answered: false, want: false},
		{name: "answer for a different question", answered: true, answersJSON: `{"answers":{"something else":"Use the agent's"}}`, want: false},
		{name: "malformed answer payload", answered: true, answersJSON: `not json`, want: false},
		{name: "rpc failure", err: errors.New("core unreachable"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &mockAsker{answered: tt.answered, answersJSON: tt.answersJSON, err: tt.err}
			if got := confirmStartCmdOverride(context.Background(), m, "bun run dev"); got != tt.want {
				t.Errorf("confirmStartCmdOverride = %v, want %v", got, tt.want)
			}
		})
	}
}

// The human deciding needs to see the command they're being asked to bless —
// approving an override sight-unseen is the same failure with an extra click.
func TestConfirmStartCmdOverride_QuestionShowsProposedCommand(t *testing.T) {
	m := &mockAsker{answered: true, answersJSON: answer(startCmdKeepProfile)}
	confirmStartCmdOverride(context.Background(), m, "bunx vite dev --host 0.0.0.0 --port $PORT")
	if !strings.Contains(m.gotPayload, "bunx vite dev --host 0.0.0.0 --port $PORT") {
		t.Errorf("question payload must contain the proposed command, got: %s", m.gotPayload)
	}
	if !strings.Contains(m.gotPayload, startCmdOverrideQuestion) {
		t.Errorf("question payload must use the constant the answer is keyed by, got: %s", m.gotPayload)
	}
}
