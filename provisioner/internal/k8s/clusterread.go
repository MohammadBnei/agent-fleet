package k8s

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
)

// ReadServiceAccount is the read-only kube identity of a definition with cluster.read (ADR-0084 §6). Its
// ClusterRole and the pods/log Roles live in infra-bootstrap.
const ReadServiceAccount = "fleet-read"

// ClusterReadLabel marks a granted pod; infra-bootstrap's CiliumNetworkPolicy admits the API server for it.
// The policy selects the whole pod, so the worker can reach the API port too, without a token: anonymous
// endpoints only, accepted.
const ClusterReadLabel = "fleet.bnei.dev/cluster-read"

// clusterReadTokenTTL is the projected token's life, a choice (the API floor is 600); the kubelet rotates the file.
const clusterReadTokenTTL = 3600

// applyClusterRead names the read ServiceAccount and mounts its token in the sidecar only. The pod keeps
// automountServiceAccountToken false, so the worker never gets one; the projected volume is the only way in.
func applyClusterRead(t *corev1.PodTemplateSpec) {
	ps := &t.Spec
	t.Labels = maps.Clone(t.Labels)
	t.Labels[ClusterReadLabel] = "true"
	ps.ServiceAccountName = ReadServiceAccount
	ttl := int64(clusterReadTokenTTL)
	ps.Volumes = append(ps.Volumes, corev1.Volume{Name: "kube-read", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &ttl}},
			{ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
				Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
		}},
	}})
	for i := range ps.InitContainers {
		if c := &ps.InitContainers[i]; c.Name == "sidecar" {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name: "kube-read", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
		}
	}
}
