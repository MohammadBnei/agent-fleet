package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	McpCallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_sidecar_mcp_calls_total",
			Help: "MCP tool calls by tool name and status",
		},
		[]string{"tool_name", "status"},
	)

	TelemetryPushesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_sidecar_telemetry_pushes_total",
			Help: "Telemetry pushes by task_id",
		},
		[]string{"task_id"},
	)

	HumanMessagesDeliveredTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_sidecar_human_messages_delivered_total",
			Help: "Human messages delivered by task_id",
		},
		[]string{"task_id"},
	)

	HeartbeatsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_sidecar_heartbeats_total",
			Help: "Heartbeats by task_id and lease validity",
		},
		[]string{"task_id", "lease_valid"},
	)
)
