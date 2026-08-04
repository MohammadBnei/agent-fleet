package config

import "os"

type Config struct {
	Namespace         string
	E2eRunnerImage    string
	E2eHost           string
	Port              string
	GRPCPort          string
	DBHost            string
	DBPort            string
	DBName            string
	DBUser            string
	DBPassword        string
	ReconcileInterval string
}

func Load() Config {
	return Config{
		Namespace:         env("NAMESPACE", "agent-fleet"),
		E2eRunnerImage:    env("E2E_RUNNER_IMAGE", "mohammaddocker/agent-fleet-e2e-runner:latest"),
		E2eHost:           env("E2E_HOST", "e2e.bnei.dev"),
		Port:              env("PORT", "8080"),
		GRPCPort:          env("GRPC_PORT", "9090"),
		DBHost:            env("AGENTFLEET_DB_HOST", "postgres.bnei.lan"),
		DBPort:            env("AGENTFLEET_DB_PORT", "5432"),
		DBName:            env("AGENTFLEET_DB_NAME", "agentfleetdb"),
		DBUser:            env("AGENTFLEET_DB_USER", "dbuser_agentfleet"),
		DBPassword:        os.Getenv("AGENTFLEET_DB_PASSWORD"),
		ReconcileInterval: env("RECONCILE_INTERVAL_MS", "10000"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
