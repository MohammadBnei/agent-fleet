package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	PodsCreatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_pods_created_total",
			Help: "Pods created by repo and type",
		},
		[]string{"repo", "type"},
	)

	PodsDeletedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_pods_deleted_total",
			Help: "Pods deleted by repo and type",
		},
		[]string{"repo", "type"},
	)

	GitOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_git_operations_total",
			Help: "Git operations by repo and operation type",
		},
		[]string{"repo", "operation"},
	)

	GitOperationDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentfleet_provisioner_git_operation_duration_seconds",
			Help:    "Git operation duration",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	GrpcRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentfleet_provisioner_grpc_requests_total",
			Help: "gRPC requests by method and status",
		},
		[]string{"method", "status"},
	)

	ReconcileDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "agentfleet_provisioner_reconcile_duration_seconds",
			Help:    "Reconciliation loop duration",
			Buckets: prometheus.DefBuckets,
		},
	)
)
