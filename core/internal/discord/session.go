// Package discord is the fleet's notification channel. It posts when a
// session needs a human, and links to the dashboard where the human answers.
//
// It used to be a second console: slash commands that created tasks, threads
// per task, free-text replies relayed into the transcript, and a
// retry/dead-letter machine behind all of it. docs/adr/0048 cut that back to
// outbound-plus-a-link, for reasons that are worth keeping written down
// because "add buttons to Discord" is a permanently tempting idea:
//
//   - **No authorization exists here.** The dashboard sits behind Traefik
//     basic-auth; this client has a bot token and a channel id, and no user
//     allowlist. An interactive control would let anyone who can see the
//     channel approve an arbitrary Bash or Edit on any session. ADR-0029
//     deliberately deleted /approve; rebuilding it here would be the same
//     gate with less checking.
//   - **A decision's identity moves.** An unanswered AskUserQuestion re-asks
//     every 60s and mints a NEW transcript entry each time, so any control
//     posted against the previous seq is silently dead — an action that
//     cannot succeed, on a surface with no way to retract it.
//   - **A pending permission's text is the tool input**, i.e.
//     JSON.stringify({tool, input}) — an Edit's file contents, a Bash command
//     line. Relaying it verbatim leaks work product into a chat channel. The
//     old discordSafeTypes allowlist existed for exactly this, and a summary
//     is what belongs here instead.
//
// So: this file renders a one-line summary and a deep link, and never the
// entry text.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

type Client struct {
	session   *discordgo.Session
	sessions  *sessions.Store
	channelID string
	// dashboardURL is the base a notification links to. Empty disables the
	// link rather than posting a broken one.
	dashboardURL string
}

func New(cfg config.Config, sessionStore *sessions.Store) (*Client, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, fmt.Errorf("discordgo.New: %w", err)
	}
	// IntentsGuilds only. IntentMessageContent is a privileged intent that
	// existed for the deleted onMessageCreate relay; nothing reads message
	// content now, and asking for a privileged scope you do not use is how
	// a bot gets a permission it can later be tricked into exercising.
	s.Identify.Intents = discordgo.IntentsGuilds

	return &Client{
		session:      s,
		sessions:     sessionStore,
		channelID:    cfg.DiscordTriggerChannel,
		dashboardURL: strings.TrimSuffix(cfg.DashboardPublicURL, "/"),
	}, nil
}

func (c *Client) Open() error  { return c.session.Open() }
func (c *Client) Close() error { return c.session.Close() }

// NotifyBlocked posts that a session is waiting on a human.
//
// `summary` is a rendered one-liner ("needs a permission decision for Edit"),
// never the transcript entry's own text — see the package comment.
func (c *Client) NotifyBlocked(ctx context.Context, sessionID, repo, title, summary string) error {
	if c.channelID == "" {
		return nil
	}
	label := title
	if label == "" {
		label = sessionID
	}
	msg := fmt.Sprintf("**%s** · `%s`\n%s", label, repo, summary)
	if c.dashboardURL != "" {
		msg += fmt.Sprintf("\n%s/sessions/%s", c.dashboardURL, sessionID)
	}
	if _, err := c.session.ChannelMessageSend(c.channelID, msg); err != nil {
		return fmt.Errorf("notify blocked: %w", err)
	}
	slog.Info("discord: notified blocked session", "sessionId", sessionID, "repo", repo)
	return nil
}

// Notify posts a plain message to the trigger channel — the path the
// Alertmanager webhook uses to say an alert fired and a proposal is waiting.
// Kept because it is the ONLY notification a firing alert produces; deleting
// the thread machinery around it would otherwise have silenced alerts
// entirely.
func (c *Client) Notify(ctx context.Context, text string) error {
	if c.channelID == "" {
		return nil
	}
	if _, err := c.session.ChannelMessageSend(c.channelID, text); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}
