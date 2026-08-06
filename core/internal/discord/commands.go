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
		{Name: "approve", Description: "Approve the current plan for this task"},
		{
			Name:        "stop",
			Description: "Abort this task",
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
}
