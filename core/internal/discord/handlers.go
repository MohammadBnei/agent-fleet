package discord

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// Legacy fallback, ported verbatim from bot/src/index.ts: `!task repo: desc`.
var legacyTaskRe = regexp.MustCompile(`^!task\s+(\S+):\s*(.+)$`)

func (c *Client) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	ctx := context.Background()

	switch data.Name {
	case "task":
		repo := data.Options[0].StringValue()
		description := data.Options[1].StringValue()
		c.startTask(ctx, s, i, repo, description)

	case "approve":
		c.withTaskFromThread(ctx, s, i, func(taskID string) {
			c.relay(ctx, taskID, "human", "approved", "approve")
		})
		respond(s, i, "Approved.")

	case "stop":
		reason := "stopped by human"
		if opt := data.GetOption("reason"); opt != nil {
			reason = opt.StringValue()
		}
		c.withTaskFromThread(ctx, s, i, func(taskID string) {
			c.relay(ctx, taskID, "human", reason, "abort")
		})
		respond(s, i, "Stopping.")

	case "e2e-kill":
		c.withTaskFromThread(ctx, s, i, func(taskID string) {
			if _, err := c.e2e.KillSession(ctx, taskID, uuid.NewString()); err != nil {
				slog.Error("e2e-kill failed", "taskId", taskID, "error", err)
			}
		})
		respond(s, i, "Kill requested.")
	}
}

func (c *Client) startTask(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, repo, description string) {
	msg, err := s.ChannelMessageSend(c.channelID, fmt.Sprintf("**New task** (%s): %s", repo, description))
	if err != nil {
		respond(s, i, "Failed to open task thread.")
		return
	}
	thread, err := s.MessageThreadStart(c.channelID, msg.ID, description, 1440)
	if err != nil {
		respond(s, i, "Failed to open task thread.")
		return
	}
	taskID, err := c.tasks.CreateTask(ctx, repo, description, c.channelID, thread.ID)
	if err != nil {
		respond(s, i, "Failed to create task.")
		return
	}
	respond(s, i, fmt.Sprintf("Task created: <#%s> (%s)", thread.ID, taskID))
}

func (c *Client) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot {
		return
	}
	ctx := context.Background()

	if strings.HasPrefix(m.Content, "!task") {
		if match := legacyTaskRe.FindStringSubmatch(m.Content); match != nil {
			repo, description := match[1], match[2]
			msg, err := s.ChannelMessageSend(c.channelID, fmt.Sprintf("**New task** (%s): %s", repo, description))
			if err != nil {
				return
			}
			thread, err := s.MessageThreadStart(c.channelID, msg.ID, description, 1440)
			if err != nil {
				return
			}
			if _, err := c.tasks.CreateTask(ctx, repo, description, c.channelID, thread.ID); err != nil {
				slog.Error("legacy !task create failed", "error", err)
			}
		}
		return
	}

	// Free-text-in-thread relay, default type "discussion" — mirrors
	// bot/src/index.ts's plain-message-in-thread behavior.
	taskID, err := c.tasks.FindTaskIDByThread(ctx, m.ChannelID)
	if err != nil || taskID == "" {
		return
	}
	c.relay(ctx, taskID, "human", m.Content, "discussion")
}

func (c *Client) withTaskFromThread(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, fn func(taskID string)) {
	taskID, err := c.tasks.FindTaskIDByThread(ctx, i.ChannelID)
	if err != nil || taskID == "" {
		respond(s, i, "This command only works inside a task thread.")
		return
	}
	fn(taskID)
}

func (c *Client) relay(ctx context.Context, taskID, from, text, msgType string) {
	if _, err := c.transcr.Append(ctx, taskID, from, text, msgType, uuid.NewString()); err != nil {
		slog.Error("relay append failed", "taskId", taskID, "error", err)
	}
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content},
	})
}
