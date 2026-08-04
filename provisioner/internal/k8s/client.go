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
	middlewareGVR   = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}
	ingressRouteGVR = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
)

type Client struct {
	Core         kubernetes.Interface
	Dynamic      dynamic.Interface
	Namespace    string
	RunnerImage  string
	WorkerImage  string
	SidecarImage string
	WorkspacePVC string
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
		RunnerImage: cfg.RunnerImage, WorkerImage: cfg.WorkerImage,
		SidecarImage: cfg.SidecarImage, WorkspacePVC: cfg.WorkspacePVC,
	}, nil
}

// Images bundles the image/PVC config New needs — named struct instead of
// four positional strings, so a call site can't silently transpose two of
// them (RunnerImage/WorkerImage/SidecarImage are string-identical types).
type Images struct {
	RunnerImage  string
	WorkerImage  string
	SidecarImage string
	WorkspacePVC string
}
