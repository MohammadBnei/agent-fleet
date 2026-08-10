package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestStaleCommands covers the gap found while deleting /approve for the
// sessions redesign (supersedes docs/adr/0021/0025's phase-boundary
// framing): ApplicationCommandCreate only upserts by name, it never
// removes a command dropped from commandDefs — a stale, still-registered
// /approve would stay selectable in Discord forever otherwise.
func TestStaleCommands(t *testing.T) {
	current := []*discordgo.ApplicationCommand{
		{Name: "task"},
		{Name: "kill"},
		{Name: "e2e-kill"},
	}
	existing := []*discordgo.ApplicationCommand{
		{ID: "1", Name: "task"},
		{ID: "2", Name: "approve"}, // dropped from commandDefs, still registered
		{ID: "3", Name: "kill"},
		{ID: "4", Name: "e2e-kill"},
	}

	stale := staleCommands(current, existing)
	if len(stale) != 1 || stale[0].Name != "approve" {
		t.Fatalf("staleCommands = %+v, want exactly [approve]", stale)
	}
}

func TestStaleCommands_NothingStale(t *testing.T) {
	current := []*discordgo.ApplicationCommand{{Name: "task"}, {Name: "kill"}}
	existing := []*discordgo.ApplicationCommand{{ID: "1", Name: "task"}, {ID: "2", Name: "kill"}}

	if stale := staleCommands(current, existing); len(stale) != 0 {
		t.Fatalf("staleCommands = %+v, want none", stale)
	}
}
