package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	TasksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_tasks_total",
			Help: "Total number of tasks by repo and status",
		},
		[]string{"repo", "status"},
	)

	TasksCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agentfleet_tasks_current",
			Help: "Current tasks by repo, status, and pod_phase",
		},
		[]string{"repo", "status", "pod_phase"},
	)

	DispatchClaimsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_dispatch_claims_total",
			Help: "Dispatch claim outcomes",
		},
		[]string{"outcome"},
	)

	TranscriptAppendsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_transcript_appends_total",
			Help: "Transcript appends by task_id, type, from",
		},
		[]string{"task_id", "type", "from"},
	)

	PodLifecycleEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_pod_lifecycle_events_total",
			Help: "Pod lifecycle events by repo and phase",
		},
		[]string{"repo", "phase"},
	)

	GrpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_grpc_requests_total",
			Help: "gRPC requests by method and status",
		},
		[]string{"method", "status"},
	)

	GrpcDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentfleet_grpc_duration_seconds",
			Help:    "gRPC request duration",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	LokiQueryDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentfleet_loki_query_duration_seconds",
			Help:    "Loki query duration",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"component"},
	)

	HeartbeatStaleTasks = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "agentfleet_heartbeat_stale_tasks",
			Help: "Tasks with stale heartbeat",
		},
	)

	IdleTimeoutCandidates = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "agentfleet_idle_timeout_candidates",
			Help: "Tasks idle beyond timeout threshold",
		},
	)
)
