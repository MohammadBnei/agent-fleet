// Package config parses core's environment. Same AGENTFLEET_DB_*
// convention worker/bot already use (see db.ts in both packages).
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port     string
	GRPCPort string
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/core/main.go.
	LogLevel              string
	DBHost                string
	DBPort                int
	DBName                string
	DBUser                string
	DBPassword            string
	DiscordBotToken       string
	DiscordTriggerChannel string
	// Alert path (docs/adr/0037). An empty token disables the webhook
	// rather than serving it unauthenticated — it creates tasks, and a
	// thot task runs an agent with cluster access.
	AlertWebhookToken  string
	ThotDiscordChannel string
	ThotRepo           string
	LokiURL               string
	ProvisionerGRPCAddr   string
	// MaxInFlight caps fleet-wide concurrent tasks (docs/adr/0019: ~5, the
	// actual human-followable ceiling, not a technical limit) — the
	// dispatch loop's own headroom check, not per-repo.
	MaxInFlight int
	// MaxTaskRetries caps ClaimNextTask's stale-heartbeat reclaim
	// (reliability-findings.md #1) — retry_count was tracked but never
	// capped before, so a task whose worker pod keeps crashing looped
	// through reclaim forever instead of eventually failing permanently.
	MaxTaskRetries int
	// StopGrace is how long dispatch.Loop waits after a Stop request before
	// force-tearing down a worker pod that hasn't gone terminal on its own —
	// long enough for a healthy pod to notice the cooperative abort message
	// and shut down cleanly, short enough that a hung/unreachable pod
	// doesn't outlive a Stop click indefinitely.
	StopGrace time.Duration
	// IdleTimeout is the sessions redesign's idle-timeout backstop (supersedes
	// docs/adr/0021/0025's phase-boundary framing) — how long a warm pod can
	// go with no real transcript activity (tasks.last_active_at, bumped by a
	// transcript.Store decorator on every Append/AppendReply, not the
	// sidecar's own git-diff-gated telemetry or the unconditional heartbeat
	// timer, neither of which prove a human is actually present) before
	// dispatch.Loop tears it down — the same TearDownSession call
	// enforceStopGrace already makes, no status change.
	IdleTimeout time.Duration
	// GarageS3Endpoint must be externally reachable, not the in-cluster
	// garage.bnei.lan host — filestore.PresignUpload/PresignDownload sign
	// the endpoint into the URL (SigV4), and the dashboard's browser can't
	// resolve a .lan hostname (docs/adr/0030).
	GarageS3Endpoint     string
	GarageFilesBucket    string
	GarageFilesAccessKey string
	GarageFilesSecret    string
}

func Load() Config {
	return Config{
		Port:                  env("CORE_PORT", "8080"),
		GRPCPort:              env("CORE_GRPC_PORT", "9090"),
		LogLevel:              env("LOG_LEVEL", "info"),
		DBHost:                env("AGENTFLEET_DB_HOST", "postgres.bnei.lan"),
		DBPort:                envInt("AGENTFLEET_DB_PORT", 5432),
		DBName:                env("AGENTFLEET_DB_NAME", "agentfleetdb"),
		DBUser:                env("AGENTFLEET_DB_USER", "dbuser_agentfleet"),
		DBPassword:            os.Getenv("AGENTFLEET_DB_PASSWORD"),
		DiscordBotToken:       os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordTriggerChannel: os.Getenv("DISCORD_TRIGGER_CHANNEL_ID"),
		AlertWebhookToken:     os.Getenv("ALERT_WEBHOOK_TOKEN"),
		ThotDiscordChannel:    os.Getenv("THOT_DISCORD_CHANNEL_ID"),
		ThotRepo:              env("THOT_REPO", "infra-bootstrap"),
		LokiURL:               env("LOKI_URL", "http://platform-loki.monitoring.svc.cluster.local:3100"),
		ProvisionerGRPCAddr:   env("PROVISIONER_GRPC_ADDR", "provisioner.agent-fleet.svc.cluster.local:9090"),
		MaxInFlight:           envInt("MAX_IN_FLIGHT_TASKS", 5),
		MaxTaskRetries:        envInt("MAX_TASK_RETRIES", 3),
		StopGrace:             time.Duration(envInt("STOP_GRACE_MS", 30000)) * time.Millisecond,
		IdleTimeout:           time.Duration(envInt("IDLE_TIMEOUT_MS", 30*60*1000)) * time.Millisecond,
		GarageS3Endpoint:      env("GARAGE_S3_ENDPOINT", "https://s3.bnei.dev"),
		GarageFilesBucket:     env("GARAGE_FILES_BUCKET", "agent-fleet-files"),
		GarageFilesAccessKey:  os.Getenv("AGENTFLEET_FILES_S3_ACCESS_KEY"),
		GarageFilesSecret:     os.Getenv("AGENTFLEET_FILES_S3_SECRET"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
