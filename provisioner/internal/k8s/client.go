// Package k8s is the client-go port of e2e-provisioner/src/k8s.ts. Same
// object shapes (labels, volumes, ports, StripPrefix paths) as the TS
// version — see docs/adr/0013. No typed client exists for traefik.io CRDs,
// so Middleware/IngressRoute go through the dynamic client.
package k8s

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	ingressRouteGVR = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
)

type Client struct {
	Core         kubernetes.Interface
	Dynamic      dynamic.Interface
	Namespace    string
	WorkerImage  string
	SidecarImage string
	WorkspacePVC string
	// SessionStorageClass names the class for per-session working volumes
	// (docs/adr/0048 §4). Empty means the cluster default — which on
	// ukubi-cluster is longhorn, not a node-local class, so this must be set
	// explicitly to get the measured node-local behaviour.
	SessionStorageClass string
	// SessionNodeSelector constrains session pods to a node carrying this
	// "key=value" label. Empty means unconstrained.
	SessionNodeSelector map[string]string
	// LogLevel is forwarded verbatim into every worker pod's sidecar and
	// worker containers (see CreateWorkerPod) — the provisioner's own
	// LOG_LEVEL is the fleet's single source of truth for it, since those
	// containers are spawned per-task rather than configured statically.
	LogLevel string
	// CoreGRPCAddr is forwarded into every worker pod's sidecar container
	// (see CreateWorkerPod), same reasoning as LogLevel: the provisioner
	// already resolves this correctly for its own ReportPodEvents calls, so
	// the sidecar should reuse that value instead of carrying its own
	// separately-hardcoded default that can silently drift out of sync with
	// wherever core's Service actually lives (e.g. prod's release-prefixed
	// name vs kind-local's unprefixed one).
	CoreGRPCAddr  string
	ThotAuthToken string
	// Where the cluster-access ingredient's kubectl shim sends argv
	// (docs/adr/0037).
	ExecutorAddr string
	// PostgresImage/RedisImage/SharedInstancePVCSize back
	// EnsureSharedInstance (docs/adr/0034) — see catalog.ServiceDef's own
	// comment for why the image tag lives in config, not the catalog.
	PostgresImage         string
	RedisImage            string
	SharedInstancePVCSize string
}

// New builds an in-cluster client. client-go's rest.InClusterConfig()
// natively trusts the projected ServiceAccount CA — unlike the TS version,
// this needs no Bun-CA workaround (see docs/adr/0013, dropping
// NODE_EXTRA_CA_CERTS/NODE_USE_SYSTEM_CA).
func New(namespace string, cfg Images) (*Client, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	core, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("core clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &Client{
		Core: core, Dynamic: dyn, Namespace: namespace,
		WorkerImage: cfg.WorkerImage,
		SidecarImage: cfg.SidecarImage, WorkspacePVC: cfg.WorkspacePVC,
		SessionStorageClass: cfg.SessionStorageClass,
		SessionNodeSelector: cfg.SessionNodeSelector,
		LogLevel: cfg.LogLevel, CoreGRPCAddr: cfg.CoreGRPCAddr,
		ThotAuthToken: cfg.ThotAuthToken,
		ExecutorAddr:  cfg.ExecutorAddr,
		PostgresImage: cfg.PostgresImage, RedisImage: cfg.RedisImage,
		SharedInstancePVCSize: cfg.SharedInstancePVCSize,
	}, nil
}

// Images bundles the image/PVC/log-level config New needs — named struct
// instead of positional strings, so a call site can't silently transpose
// two of them (WorkerImage/SidecarImage are string-identical
// types).
type Images struct {
	WorkerImage           string
	SidecarImage          string
	WorkspacePVC          string
	SessionStorageClass   string
	SessionNodeSelector   map[string]string
	LogLevel              string
	CoreGRPCAddr          string
	ThotAuthToken         string
	ExecutorAddr          string
	PostgresImage         string
	RedisImage            string
	SharedInstancePVCSize string
}
