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
	// answer: how many tasks sit in each status right now. It's what makes
	// "N tasks stuck pending for 30m" an alertable expression.
	//
	// Labelled by repo, never by task_id — the repos table is small and
	// human-managed, task IDs are unbounded.
	TasksCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "agentfleet_tasks_current",
			Help: "Tasks currently in each status, by repo",
		},
		[]string{"repo", "status"},
	)

	// LivePods/MaxInFlight are the dispatch-headroom pair. A backlog that
	// isn't draining is LivePods == MaxInFlight with TasksCurrent{status=
	// "pending"} > 0; a stalled dispatcher is pending > 0 with headroom to
	// spare. One gauge alone distinguishes neither.
	LivePods = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "agentfleet_live_pods",
			Help: "Tasks holding a live worker pod (the in-flight count dispatch gates on)",
		},
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
