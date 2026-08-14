// Package metrics is the provisioner's Prometheus surface, served at
// /metrics and scraped via k8s/provisioner/servicemonitor.yaml.
//
// Scoped the same way core's is (docs/adr/0047): only what neither Loki nor
// kube-state-metrics already answers. Pod counts and phases come from
// kube-state-metrics; what it can't tell you is which of those pods this
// component *meant* to create, or how long the git work in front of a spawn
// took — a slow clone is invisible as a pod metric because the pod doesn't
// exist yet.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

var (
	GrpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_grpc_requests_total",
			Help: "ProvisionerService RPCs, by method and status code",
		},
		[]string{"method", "status"},
	)

	GrpcDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentfleet_provisioner_grpc_duration_seconds",
			Help:    "ProvisionerService handler duration, by method",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	// Intent, not observed state: a created_total that outruns
	// kube_pod_created is the signature of pods being made and immediately
	// failing. No repo label — the provisioner holds no bound on repo
	// cardinality (core does), and `type` is the axis that matters here.
	PodsCreatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_pods_created_total",
			Help: "Pods the provisioner created, by type (worker|e2e)",
		},
		[]string{"type"},
	)

	PodsDeletedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_pods_deleted_total",
			Help: "Pods the provisioner deleted, by type (worker|e2e)",
		},
		[]string{"type"},
	)

	// Observed in Manager.run, the single choke point every git command in
	// the package goes through — instrumenting the callers instead would be
	// six edits that each drift independently.
	GitOperationDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agentfleet_provisioner_git_operation_duration_seconds",
			Help: "git subprocess duration, by subcommand",
			// A cold clone of a large repo runs well past DefBuckets' 10s
			// ceiling, and that slow tail is the whole reason to measure
			// this — the default buckets would collapse it into +Inf.
			Buckets: []float64{0.05, 0.25, 1, 5, 15, 30, 60, 120, 300},
		},
		[]string{"operation"},
	)
)

// UnaryInterceptor instruments ProvisionerService. Chained after
// grpcserver.AccessLogInterceptor, which stays the per-call record.
func UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	GrpcRequestsTotal.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
	GrpcDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
	return resp, err
}

// ObserveGit records one git subprocess, labelled by args[0] — the
// subcommand alone. Takes the whole argv so the caller can `defer` it
// without indexing (and without panicking on an empty argv).
func ObserveGit(args []string, start time.Time) {
	operation := "unknown"
	if len(args) > 0 {
		operation = args[0]
	}
	GitOperationDuration.WithLabelValues(operation).Observe(time.Since(start).Seconds())
}
