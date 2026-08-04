// Package discord folds bot/'s Discord ingress (slash commands, thread
// relay) into core — no separate Deployment, since it shares
// core's trust boundary (no cluster RBAC) with transcript
// coordination and log/introspection queries (see docs/adr/0013).
package discord

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Client struct {
	session   *discordgo.Session
	tasks     *tasks.Store
	transcr   transcript.Store
	e2e       *provisionerclient.Client
	guildID   string
	channelID string
	appID     string
}

func New(cfg config.Config, taskStore *tasks.Store, transcr transcript.Store, e2e *provisionerclient.Client) (*Client, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, fmt.Errorf("discordgo.New: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	c := &Client{
		session:   s,
		tasks:     taskStore,
		transcr:   transcr,
		e2e:       e2e,
		channelID: cfg.DiscordTriggerChannel,
	}
	s.AddHandler(c.onInteractionCreate)
	s.AddHandler(c.onMessageCreate)
	s.AddHandler(c.onReady)
	return c, nil
}

func (c *Client) Open() error {
	return c.session.Open()
}

func (c *Client) Close() error {
	return c.session.Close()
}

func (c *Client) onReady(s *discordgo.Session, r *discordgo.Ready) {
	c.appID = r.Application.ID

	// Guild-scoped registration, derived from the trigger channel (mirrors
	// bot/src/index.ts) — not global, so command updates propagate
	// immediately instead of Discord's up-to-1h global-command cache.
	ch, err := s.Channel(c.channelID)
	if err != nil {
		return
	}
	c.guildID = ch.GuildID
	registerCommands(s, c.appID, c.guildID)
}

// PostToThread implements transcript.Notifier — the relay loop's Discord
// side effect.
func (c *Client) PostToThread(ctx context.Context, taskID string, e transcript.Entry) error {
	t, err := c.tasks.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t == nil || t.ThreadID == nil {
		return nil
	}
	_, err = c.session.ChannelMessageSend(*t.ThreadID, fmt.Sprintf("**%s**: %s", e.From, e.Text))
	return err
}
