package coreserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The budget reaches the agent as words only (the sidecar relays err text),
// so its message must start with its key.
func TestDraftCardErr(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		code   codes.Code
		key    string
		prefix string
	}{
		{"budget", fmt.Errorf("append: %w", decisions.ErrCardBudget), codes.ResourceExhausted, string(errkeys.KeyCardBudget), "core.card_budget:"},
		{"invalid", fmt.Errorf("%w: ask is empty", decisions.ErrInvalid), codes.InvalidArgument, "", "invalid answer: ask is empty"},
		{"other", errors.New("db down"), codes.Internal, "", "draft card: db down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := draftCardErr(tc.err)
			s, _ := status.FromError(err)
			if s.Code() != tc.code || string(errkeys.Of(err)) != tc.key || !strings.HasPrefix(s.Message(), tc.prefix) {
				t.Errorf("got %v %q %q", s.Code(), errkeys.Of(err), s.Message())
			}
		})
	}
}

// A card is core's to write: SendMessage refuses one before it reaches the
// transcript.
func TestSendMessage_RefusesCard(t *testing.T) {
	_, err := (&Server{}).SendMessage(context.Background(), &agentfleetv1.SendMessageRequest{
		SessionId: "s1", From: "agent", Text: "{}", Type: agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_CARD,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got %v, want InvalidArgument", err)
	}
}
