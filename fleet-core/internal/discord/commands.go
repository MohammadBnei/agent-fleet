package discord

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/tasks"
)

func repoChoices() []*discordgo.ApplicationCommandOptionChoice {
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(tasks.KnownRepos))
	for _, r := range tasks.KnownRepos {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: r, Value: r})
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
			{Type: discordgo.ApplicationCommandOptionBoolean, Name: "skip_critique", Description: "Skip the critic phase"},
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
