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
		slog.Error("startTask: channel message send failed", "channelId", c.channelID, "error", err)
		respond(s, i, "Failed to open task thread.")
		return
	}
	thread, err := s.MessageThreadStart(c.channelID, msg.ID, threadName(repo, description), 1440)
	if err != nil {
		slog.Error("startTask: thread start failed", "channelId", c.channelID, "messageId", msg.ID, "error", err)
		respond(s, i, "Failed to open task thread.")
		return
	}
	taskID, err := c.tasks.CreateTask(ctx, repo, description, "", &c.channelID, &thread.ID)
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
				slog.Error("legacy !task: channel message send failed", "channelId", c.channelID, "error", err)
				return
			}
			thread, err := s.MessageThreadStart(c.channelID, msg.ID, threadName(repo, description), 1440)
			if err != nil {
				slog.Error("legacy !task: thread start failed", "channelId", c.channelID, "messageId", msg.ID, "error", err)
				return
			}
			if _, err := c.tasks.CreateTask(ctx, repo, description, "", &c.channelID, &thread.ID); err != nil {
				slog.Error("legacy !task create failed", "error", err)
			}
		}
		return
	}

	// Free-text-in-thread relay — mirrors bot/src/index.ts's plain-message-
	// in-thread behavior, but a reply to a still-pending AskUserQuestion
	// call gets tagged "answer" with question-seq correlation instead of
	// defaulting to "discussion" (reliability-findings.md #0: before this,
	// Discord users had no way to actually answer a question — only the
	// dashboard's AnswerQuestion RPC could).
	taskID, err := c.tasks.FindTaskIDByThread(ctx, m.ChannelID)
	if err != nil || taskID == "" {
		return
	}
	if pendingSeq, err := c.findPendingQuestionSeq(ctx, taskID); err != nil {
		slog.Error("find pending question failed", "taskId", taskID, "error", err)
	} else if pendingSeq > 0 {
		if _, err := c.transcr.AppendReply(ctx, taskID, "human", m.Content, "answer", uuid.NewString(), pendingSeq); err != nil {
			slog.Error("relay reply append failed", "taskId", taskID, "error", err)
		}
		return
	}
	c.relay(ctx, taskID, "human", m.Content, "discussion")
}

// findPendingQuestionSeq returns the seq of the most recent still-open
// "question" entry for taskID, or 0 if none is pending. "Open" means no
// later "answer" entry replies to it — AskUserQuestion's own matching
// loop (coreserver.Server.AskUserQuestion) uses the identical ReplyTo
// correlation, so this stays consistent with what actually unblocks it.
func (c *Client) findPendingQuestionSeq(ctx context.Context, taskID string) (int64, error) {
	entries, _, err := c.transcr.ReadSince(ctx, taskID, 0, 1000)
	if err != nil {
		return 0, err
	}
	answered := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if e.Type == "answer" && e.ReplyTo != nil {
			answered[*e.ReplyTo] = true
		}
	}
	var pending int64
	for _, e := range entries {
		if e.Type == "question" && !answered[e.Seq] {
			pending = e.Seq
		}
	}
	return pending, nil
}

func (c *Client) withTaskFromThread(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, fn func(taskID string)) {
	taskID, err := c.tasks.FindTaskIDByThread(ctx, i.ChannelID)
	if err != nil || taskID == "" {
		respond(s, i, "This command only works inside a task thread.")
		return
	}
	fn(taskID)
}

// threadName mirrors bot/src/index.ts's `${repo}: ${description.slice(0, 80)}`
// — Discord thread names are capped at 100 chars, so an untruncated
// description makes MessageThreadStart fail with no other symptom.
func threadName(repo, description string) string {
	r := []rune(description)
	if len(r) > 80 {
		description = string(r[:80])
	}
	return repo + ": " + description
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
