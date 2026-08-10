package discord

import (
	"log/slog"
	"sort"

	"github.com/bwmarrin/discordgo"
)

// repoNames is unsorted-DB-order input, sorted here — a DB-order range
// would make the /task command's repo dropdown flap across refreshes
// (docs/adr/0028: repos now come from the DB-backed repos table, not the
// old static tasks.KnownRepos map).
func repoChoices(repoNames []string) []*discordgo.ApplicationCommandOptionChoice {
	names := append([]string(nil), repoNames...)
	sort.Strings(names)

	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(names))
	for _, repo := range names {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: repo, Value: repo})
	}
	return choices
}

// commandDefs is built per-call, not a package var, since the /task
// command's repo choices now depend on a DB read (docs/adr/0028) rather
// than a value known at process-init time.
func commandDefs(repoNames []string) []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "task",
			Description: "Start a new agent-fleet task",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "repo", Description: "Target repo", Required: true, Choices: repoChoices(repoNames)},
				{Type: discordgo.ApplicationCommandOptionString, Name: "description", Description: "What to do", Required: true},
			},
		},
		{
			Name:        "kill",
			Description: "Kill this task's session",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "reason", Description: "Why"},
			},
		},
		{Name: "e2e-kill", Description: "Tear down this task's on-demand e2e environment"},
	}
}

// registerCommands upserts every command by name (ApplicationCommandCreate
// replaces an existing command of the same name) — safe to call again after
// startup, which is how RefreshCommands (session.go) live-updates the /task
// repo dropdown after a repos-table mutation, no restart needed.
func registerCommands(s *discordgo.Session, appID, guildID string, repoNames []string) {
	for _, cmd := range commandDefs(repoNames) {
		if _, err := s.ApplicationCommandCreate(appID, guildID, cmd); err != nil {
			slog.Error("register command failed", "command", cmd.Name, "error", err)
		}
	}
	pruneStaleCommands(s, appID, guildID, commandDefs(repoNames))
}

// pruneStaleCommands deletes any guild command Discord still has
// registered that commandDefs no longer lists — ApplicationCommandCreate
// only upserts by name, it never removes one dropped from the list (found
// while deleting /approve for the sessions redesign, supersedes docs/adr/
// 0021/0025's phase-boundary framing: leaving the old definition alone
// only stops core from handling it server-side, the stale command stays
// visible and selectable in Discord, worse than not registering it at
// all). Runs after every registerCommands call, not just once at startup,
// so a repos-table-triggered RefreshCommands also catches a command
// deleted from commandDefs in a later deploy.
func pruneStaleCommands(s *discordgo.Session, appID, guildID string, current []*discordgo.ApplicationCommand) {
	existing, err := s.ApplicationCommands(appID, guildID)
	if err != nil {
		slog.Error("prune stale commands: list failed", "error", err)
		return
	}
	for _, cmd := range staleCommands(current, existing) {
		if err := s.ApplicationCommandDelete(appID, guildID, cmd.ID); err != nil {
			slog.Error("prune stale command failed", "command", cmd.Name, "error", err)
			continue
		}
		slog.Info("pruned stale slash command", "command", cmd.Name)
	}
}

// staleCommands is pruneStaleCommands' pure decision logic, split out so
// it's testable without a real discordgo.Session (same reasoning
// session.go's channelWithRetry is a standalone function rather than a
// method) — every command in `existing` whose name doesn't appear in
// `current`.
func staleCommands(current, existing []*discordgo.ApplicationCommand) []*discordgo.ApplicationCommand {
	want := make(map[string]bool, len(current))
	for _, cmd := range current {
		want[cmd.Name] = true
	}
	var stale []*discordgo.ApplicationCommand
	for _, cmd := range existing {
		if !want[cmd.Name] {
			stale = append(stale, cmd)
		}
	}
	return stale
}
