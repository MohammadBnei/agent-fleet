// Package discord folds bot/'s Discord ingress (slash commands, thread
// relay) into core — no separate Deployment, since it shares
// core's trust boundary (no cluster RBAC) with transcript
// coordination and log/introspection queries (see docs/adr/0013).
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Client struct {
	session   *discordgo.Session
	tasks     *tasks.Store
	transcr   transcript.Store
	repos     *repos.Store
	e2e       *provisionerclient.Client
	guildID   string
	channelID string
	appID     string
}

func New(cfg config.Config, taskStore *tasks.Store, transcr transcript.Store, repoStore *repos.Store, e2e *provisionerclient.Client) (*Client, error) {
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, fmt.Errorf("discordgo.New: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent

	c := &Client{
		session:   s,
		tasks:     taskStore,
		transcr:   transcr,
		repos:     repoStore,
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
	//
	// Retried: a transient failure here used to be silent and permanent —
	// onReady only fires once per connection, so one bad REST call meant
	// slash commands never registered until the next pod restart, with
	// nothing logged anywhere to explain why.
	ch, err := channelWithRetry(func() (*discordgo.Channel, error) { return s.Channel(c.channelID) }, 3, 2*time.Second)
	if err != nil {
		slog.Error("onReady: channel lookup failed, giving up — slash commands not registered", "channelId", c.channelID, "error", err)
		return
	}
	c.guildID = ch.GuildID
	c.RefreshCommands(context.Background())
}

// RefreshCommands re-fetches the repo list and re-registers every slash
// command — discordgo.ApplicationCommandCreate upserts by name, so calling
// this again live-updates /task's repo dropdown with no restart needed.
// Wired as repos.Store's OnChange callback (core/cmd/core/run.go) so a
// dashboard repo add/edit/delete propagates immediately (docs/adr/0028); a
// no-op if onReady hasn't fired yet (guildID/appID still empty).
func (c *Client) RefreshCommands(ctx context.Context) {
	if c.appID == "" || c.guildID == "" {
		return
	}
	list, err := c.repos.List(ctx)
	if err != nil {
		slog.Error("RefreshCommands: repo list failed, commands not refreshed", "error", err)
		return
	}
	names := make([]string, len(list))
	for i, r := range list {
		names[i] = r.Name
	}
	registerCommands(c.session, c.appID, c.guildID, names)
}

// channelWithRetry retries a transient Discord REST failure a few times
// before giving up, logging each attempt so a startup hiccup is visible
// instead of silently dropping command registration forever.
func channelWithRetry(lookup func() (*discordgo.Channel, error), attempts int, delay time.Duration) (*discordgo.Channel, error) {
	var ch *discordgo.Channel
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		ch, err = lookup()
		if err == nil {
			return ch, nil
		}
		slog.Error("onReady: channel lookup failed, retrying", "attempt", attempt, "error", err)
		if attempt < attempts {
			time.Sleep(delay)
		}
	}
	return nil, err
}

// PostToThread implements transcript.Notifier — the relay loop's Discord
// side effect.
// OpenThread posts a message to channelID and starts a thread on it,
// returning the thread id. Used by machinery-created thot tasks (alerts,
// audits) so their findings stream into Discord through the SAME relay a
// human-created task uses — rather than a second, notify-only channel
// with its own bot and its own failure modes (which is what docs/adr/0035
// originally specified, and docs/adr/0037 made unnecessary).
//
// channelID falls back to the trigger channel when empty, so a fleet that
// hasn't configured a separate thot channel still gets the notifications
// somewhere visible instead of silently dropping them.
func (c *Client) OpenThread(channelID, title, body string) (string, error) {
	if channelID == "" {
		channelID = c.channelID
	}
	if channelID == "" {
		return "", fmt.Errorf("no Discord channel configured")
	}
	msg, err := c.session.ChannelMessageSend(channelID, body)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	thread, err := c.session.MessageThreadStart(channelID, msg.ID, title, 1440)
	if err != nil {
		return "", fmt.Errorf("start thread: %w", err)
	}
	return thread.ID, nil
}

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
