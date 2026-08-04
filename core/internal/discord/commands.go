package discord

import (
	"log/slog"
	"sort"

	"github.com/bwmarrin/discordgo"

	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// Sorted, not map-iteration order — tasks.KnownRepos became a map when it
// grew per-repo config (docs/adr/0019), and an unsorted range would make
// the /task command's repo dropdown order flap across restarts.
func repoChoices() []*discordgo.ApplicationCommandOptionChoice {
	repos := make([]string, 0, len(tasks.KnownRepos))
	for repo := range tasks.KnownRepos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(repos))
	for _, repo := range repos {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: repo, Value: repo})
	}
	return choices
}

var commandDefs = []*discordgo.ApplicationCommand{
	{
		Name:        "task",
		Description: "Start a new agent-fleet task",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "repo", Description: "Target repo", Required: true, Choices: repoChoices()},
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

func registerCommands(s *discordgo.Session, appID, guildID string) {
	for _, cmd := range commandDefs {
		if _, err := s.ApplicationCommandCreate(appID, guildID, cmd); err != nil {
			slog.Error("register command failed", "command", cmd.Name, "error", err)
		}
	}
}
