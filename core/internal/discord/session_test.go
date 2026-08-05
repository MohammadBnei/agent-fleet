package discord

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestChannelWithRetry_SucceedsAfterTransientFailure(t *testing.T) {
	calls := 0
	want := &discordgo.Channel{ID: "chan-1"}
	got, err := channelWithRetry(func() (*discordgo.Channel, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("transient")
		}
		return want, nil
	}, 3, time.Millisecond)

	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got error: %v", err)
	}
	if got != want || calls != 3 {
		t.Fatalf("got channel %v after %d calls, want %v after 3 calls", got, calls, want)
	}
}

func TestChannelWithRetry_GivesUpAfterAllAttemptsFail(t *testing.T) {
	calls := 0
	wantErr := errors.New("persistent")
	_, err := channelWithRetry(func() (*discordgo.Channel, error) {
		calls++
		return nil, wantErr
	}, 3, time.Millisecond)

	if !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want %v", err, wantErr)
	}
	if calls != 3 {
		t.Fatalf("got %d attempts, want 3", calls)
	}
}
