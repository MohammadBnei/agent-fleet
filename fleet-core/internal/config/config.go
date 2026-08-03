// Package config parses fleet-core's environment. Same AGENTFLEET_DB_*
// convention worker/bot already use (see db.ts in both packages).
package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port                   string
	DBHost                 string
	DBPort                 int
	DBName                 string
	DBUser                 string
	DBPassword             string
	DiscordBotToken        string
	DiscordTriggerChannel  string
	LokiURL                string
	E2eProvisionerGRPCAddr string
}

func Load() Config {
	return Config{
		Port:                   env("FLEET_CORE_PORT", "8080"),
		DBHost:                 env("AGENTFLEET_DB_HOST", "postgres.bnei.lan"),
		DBPort:                 envInt("AGENTFLEET_DB_PORT", 5432),
		DBName:                 env("AGENTFLEET_DB_NAME", "agentfleetdb"),
		DBUser:                 env("AGENTFLEET_DB_USER", "dbuser_agentfleet"),
		DBPassword:             os.Getenv("AGENTFLEET_DB_PASSWORD"),
		DiscordBotToken:        os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordTriggerChannel:  os.Getenv("DISCORD_TRIGGER_CHANNEL_ID"),
		LokiURL:                env("LOKI_URL", "http://loki.monitoring.svc.cluster.local:3100"),
		E2eProvisionerGRPCAddr: env("E2E_PROVISIONER_GRPC_ADDR", "e2e-provisioner.agent-fleet.svc.cluster.local:9090"),
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
