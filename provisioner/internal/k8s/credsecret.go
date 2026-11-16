package k8s

import (
	"context"
	"fmt"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// docs/adr/0086 §6: no credential in any pod's env. The per-warm lease, vault
// session token and executor token live in one per-pod Secret, mounted as
// files (no subPath, so kubelet refreshes them), and env holds the paths.
const (
	credDir = "/run/fleet/cred"

	credKeyLease      = "lease"
	credKeyVaultProxy = "vault-proxy"
	credKeyThot       = "thot-token"

	// Path env the sidecar and the kubectl shim read (the value is a path).
	LeaseIDFileEnv       = "LEASE_ID_FILE"
	VaultProxyFileEnv    = "FLEET_VAULT_PROXY_FILE"
	ThotAuthTokenFileEnv = "THOT_AUTH_TOKEN_FILE"
)

// CredSecretName is a session's credential Secret.
func CredSecretName(taskID string) string { return WorkerResourceName(taskID) + "-cred" }

// credVolume projects the named keys of the pod's Secret. One volume per
// container, so the worker never sees the lease or the vault token and the
// sidecar never sees the executor token.
func credVolume(name, secret string, keys ...string) (corev1.Volume, corev1.VolumeMount) {
	items := make([]corev1.KeyToPath, len(keys))
	for i, k := range keys {
		items[i] = corev1.KeyToPath{Key: k, Path: k}
	}
	mode := int32(0o440) // group-readable: fsGroup 1000 is the containers' group
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secret, Items: items, DefaultMode: &mode,
		}}},
		corev1.VolumeMount{Name: name, MountPath: credDir, ReadOnly: true}
}

// createCredSecret writes the pod's Secret before its Job. A leftover from a
// previous warm (its Job's delete may not have collected it yet) is replaced,
// owner reference dropped, so the old Job's collection cannot delete it.
func (c *Client) createCredSecret(ctx context.Context, taskID string, data map[string]string) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredSecretName(taskID), Namespace: c.Namespace, Labels: map[string]string{TaskIDLabel: taskID}},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
	secrets := c.Core.CoreV1().Secrets(c.Namespace)
	_, err := secrets.Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
	}
	return err
}

// adoptCredSecret makes the Job the Secret's owner, so the Job's deletion
// (teardown, TTL, reconcile) collects it.
func (c *Client) adoptCredSecret(ctx context.Context, taskID string, job *batchv1.Job) error {
	ref := fmt.Sprintf(`{"metadata":{"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":%q,"uid":%q}]}}`, job.Name, job.UID)
	_, err := c.Core.CoreV1().Secrets(c.Namespace).Patch(ctx, CredSecretName(taskID), types.MergePatchType, []byte(ref), metav1.PatchOptions{})
	return err
}

// deleteCredSecret is best effort: the owner reference is the real cleanup.
func (c *Client) deleteCredSecret(ctx context.Context, taskID string) {
	if err := ignoreNotFound(c.Core.CoreV1().Secrets(c.Namespace).Delete(ctx, CredSecretName(taskID), metav1.DeleteOptions{})); err != nil {
		slog.Warn("k8s deleteCredSecret", "sessionId", taskID, "error", err)
	}
}
