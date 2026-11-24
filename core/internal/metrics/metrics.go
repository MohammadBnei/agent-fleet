// Package metrics is core's Prometheus surface, served at /metrics and
// scraped via the ServiceMonitor in k8s/core.yaml's extraManifests.
//
// Deliberately small. Anything already answerable from Loki (every RPC is
// access-logged; the worker logs its own SDK turns/cost/token counts) or
// from kube-state-metrics/cAdvisor (pod counts, phases, restarts, CPU,
// memory) is NOT duplicated here — see docs/adr/0047. What's left is the
// part nothing else knows: core's own view of the task queue, and RPC
// latency as a distribution rather than a pile of log lines.
//
// Every metric below is incremented somewhere. A declared-but-never-touched
// metric renders as absent and reads as "the feature is broken" — which is
// exactly how the first attempt at this shipped.
package metrics

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

var (
	// UncontractedDropped counts events core acked and dropped because the
	// bridge flagged their type as missing from its contract. Any non-zero
	// rate means contract drift: work core would have acted on is being lost.
	UncontractedDropped = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_dispatch_uncontracted_dropped_total",
			Help: "Bridge events acked and dropped as uncontracted, by event type",
		},
		[]string{"type"},
	)

	// ResponderStartFailed counts alert groups whose first-responder run could not start (docs/adr/0087). The alert
	// then reaches no session, only Discord and the bridge log; any increase is alertable.
	ResponderStartFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_responder_start_failed_total",
			Help: "Responder runs that could not start, by definition; the alert reaches no session",
		},
		[]string{"definition"},
	)

	// Card events (core/internal/cardevents, docs/adr/0078 amendment). Enabled is 1 when the bridge client is
	// configured; Pending counts the card rows whose state the bridge has not accepted yet; LastSuccess is seeded
	// at start and renewed only when the bridge answers a publish. CardEventsOff and CardEventsStuck alert on
	// Enabled and Pending (infra-bootstrap's agent-fleet rules).
	CardEventsEnabled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_card_events_enabled",
		Help: "1 when core's bridge client is configured, so card events can be published",
	})
	CardEventsPublished = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_card_events_published_total",
			Help: "Card events the bridge accepted (or already had), by event type",
		},
		[]string{"type"},
	)
	CardEventsRefused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentfleet_card_events_refused_total",
		Help: "Card events the bridge refused as invalid; the card is parked, never retried",
	})
	CardEventsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_card_events_pending",
		Help: "Card rows whose state the bridge has not accepted yet",
	})
	CardEventsParked = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_card_events_parked",
		Help: "Card rows the bridge refused (InvalidArgument, PermissionDenied) and core no longer retries",
	})
	CardEventsLost = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentfleet_card_events_lost_total",
		Help: "Open cards already published to the bridge that a session hard delete removed, with no terminal event",
	})
	CardEventsLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_card_events_last_success_timestamp_seconds",
		Help: "When the bridge last answered a card event publish (seeded at start); ages while no card changes",
	})

	// Session events (core/internal/sessionevents): opened, turn_ended, ended. Same meaning as the card gauges.
	SessionEventsEnabled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_session_events_enabled",
		Help: "1 when core's bridge client is configured, so session events can be published",
	})
	SessionEventsPublished = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_session_events_published_total",
			Help: "Session events the bridge accepted (or already had), by event type",
		},
		[]string{"type"},
	)
	SessionEventsRefused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentfleet_session_events_refused_total",
		Help: "Session events the bridge refused as invalid; the session is parked, never retried",
	})
	SessionEventsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_session_events_pending",
		Help: "Sessions with a state change or a result entry the bridge has not accepted yet",
	})
	SessionEventsParked = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_session_events_parked",
		Help: "Sessions the bridge refused (InvalidArgument, PermissionDenied) and core no longer publishes for",
	})
	SessionEventsLost = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentfleet_session_events_lost_total",
		Help: "Sessions the bridge saw open that a hard delete removed, with no ended event",
	})
	SessionEventsLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "agentfleet_session_events_last_success_timestamp_seconds",
		Help: "When the bridge last answered a session event publish (seeded at start); ages while no session changes",
	})

	// GrpcRequestsTotal/GrpcDuration cover both RPC surfaces core serves —
	// CoreService (gRPC, sidecar + provisioner call it) and DashboardService
	// (ConnectRPC, the SPA calls it) — separated by the `surface` label
	// rather than by two metric families, so one panel shows both.
	GrpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_grpc_requests_total",
			Help: "RPCs served by core, by surface, method and status code",
		},
		[]string{"surface", "method", "status"},
	)

	GrpcDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentfleet_grpc_duration_seconds",
			Help:    "RPC handler duration, by surface and method",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"surface", "method"},
	)

	// TasksCurrent is the one thing neither Loki nor kube-state-metrics can
	// answer: how many sessions are in each live state right now
	// (sessions.CountByRepoLiveState: working, blocked, stalled, idle…). It's
	// what makes "N sessions blocked for 30m" an alertable expression.
	//
	// The name and the `status` label predate the tasks→sessions rename
	// (docs/adr/0048) and are kept so existing queries keep working; the
	// values are live states now, never the deleted status enum.
	//
	// Labelled by repo, never by session id — repos are few, sessions are
	// unbounded.
	TasksCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agentfleet_tasks_current",
			Help: "Sessions currently in each live state (label `status`), by repo",
		},
		[]string{"repo", "status"},
	)

	// LivePods/MaxInFlight are the warm-headroom pair. There is no queue
	// (docs/adr/0048 §2): at LivePods == MaxInFlight the next warm is
	// refused, not queued, so the pair is what says "the fleet is full".
	LivePods = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "agentfleet_live_pods",
			Help: "Tasks holding a live worker pod (the in-flight count dispatch gates on)",
		},
	)

	// CharacterTurnsLastHour and CharacterTurnsPerDay are the pair behind ADR-0084 §9's alert: turns_last_hour * 24
	// >= turnsPerDay / 2 means the character spends half its day's budget at this hour's pace. Labelled by
	// character (a definition name, few).
	CharacterTurnsLastHour = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agentfleet_character_turns_last_hour",
			Help: "Turns a started standing character finished in the last hour",
		},
		[]string{"character"},
	)

	CharacterTurnsPerDay = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agentfleet_character_turns_per_day",
			Help: "A started standing character's limits.turnsPerDay",
		},
		[]string{"character"},
	)

	MaxInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "agentfleet_max_in_flight",
			Help: "Configured fleet-wide concurrency cap (MAX_IN_FLIGHT_TASKS)",
		},
	)
)

// SetTasksCurrent replaces the whole TasksCurrent gauge family from one
// snapshot. Reset first, because a (repo, status) pair that no longer has
// any tasks would otherwise keep reporting its last non-zero value forever —
// a gauge only ever hears about combinations that still exist.
func SetTasksCurrent(counts map[[2]string]int) {
	TasksCurrent.Reset()
	for key, n := range counts {
		TasksCurrent.WithLabelValues(key[0], key[1]).Set(float64(n))
	}
}

// UnaryInterceptor instruments CoreService. Chained after (not instead of)
// coreserver.AccessLogInterceptor — the log line stays the per-call record,
// this is the aggregate.
func UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	observe("core", info.FullMethod, status.Code(err).String(), start)
	return resp, err
}

// connectInterceptor instruments DashboardService, including its streaming
// handlers — StreamTranscript is the RPC most likely to misbehave, so
// leaving it out would hide the interesting half.
type connectInterceptor struct{}

// NewConnectInterceptor returns the DashboardService counterpart to
// UnaryInterceptor.
func NewConnectInterceptor() connect.Interceptor { return connectInterceptor{} }

func (connectInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		observe("dashboard", req.Spec().Procedure, connectCode(err), start)
		return resp, err
	}
}

// core is never its own client — passthrough, same as the CSRF and access-log
// interceptors next door.
func (connectInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (connectInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		observe("dashboard", conn.Spec().Procedure, connectCode(err), start)
		return err
	}
}

// connect.CodeOf(nil) is CodeUnknown, not "no error" — calling it
// unguarded labels every successful RPC as `unknown` and makes the error
// rate look like 100%.
func connectCode(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

func observe(surface, method, code string, start time.Time) {
	GrpcRequestsTotal.WithLabelValues(surface, method, code).Inc()
	GrpcDuration.WithLabelValues(surface, method).Observe(time.Since(start).Seconds())
}
