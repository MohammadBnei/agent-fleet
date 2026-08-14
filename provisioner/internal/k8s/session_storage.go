package k8s

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sessionWorkdir is where a session's working tree is mounted, and it is the
// same path for every session — which is a change with a consequence worth
// stating (docs/adr/0048 §4).
//
// The Agent SDK derives its per-project state directory from `cwd`, replacing
// every non-alphanumeric character with `-`. Under the old layout each cwd was
// a distinct worktree path, so those directories were naturally disjoint. Now
// every session's cwd is `/workspace`, so every session encodes to the same
// `projects/-workspace/`. The per-session claude-home MOUNT is what keeps them
// apart; without it, resume state from different sessions would land in one
// directory and the SDK would replay the wrong conversation.
const sessionWorkdir = "/workspace"

// sessionPVCSize bounds one session's working tree plus its dependency caches.
// Generous because it holds node_modules and a Go build cache, and because
// local-path storage is the node's own disk rather than a replicated pool.
const sessionPVCSize = "20Gi"

// SessionPVCName is the per-session working volume's name. Derived, not
// stored: Kubernetes is the source of truth for whether it exists, and a
// derived name means the GC can delete one without first looking up a row.
func SessionPVCName(sessionID string) string {
	return "session-" + shortID(sessionID)
}

// EnsureSessionPVC creates the session's working volume if it is absent.
//
// Idempotent by AlreadyExists rather than by a get-then-create: two warms of
// the same session can race, and the second one finding the volume already
// there is the normal case, not an error. It is also what makes a resumed
// session reuse its tree instead of re-cloning.
//
// StorageClassName is deliberately left empty so the cluster's default class
// applies. On ukubi-cluster that is `longhorn`, NOT `local-path` — see the
// caveat in the ADR: this wants a WaitForFirstConsumer node-local class, and
// naming one that does not exist in a given cluster would leave every session
// Pending forever. Set SESSION_STORAGE_CLASS to pin it.
func (c *Client) EnsureSessionPVC(ctx context.Context, sessionID, repo string) error {
	name := SessionPVCName(sessionID)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.Namespace,
			Labels:    WorkerLabels(sessionID, repo),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// RWO is correct and not a limitation: exactly one pod ever mounts
			// this volume, because a session has at most one pod.
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(sessionPVCSize),
				},
			},
		},
	}
	if c.SessionStorageClass != "" {
		pvc.Spec.StorageClassName = &c.SessionStorageClass
	}

	_, err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		slog.Error("k8s EnsureSessionPVC", "sessionId", sessionID, "pvc", name, "error", err)
		return fmt.Errorf("create session pvc: %w", err)
	}
	slog.Info("k8s EnsureSessionPVC", "sessionId", sessionID, "pvc", name)
	return nil
}

// DeleteSessionPVC reclaims a session's working volume. This is half of the
// retention GC; the other half is the SDK state directory on the shared
// volume, removed by the git manager.
//
// Deleting a PVC that does not exist is a correct no-op — the GC retries a
// failed sweep on its next pass, and by then one half may already be gone.
func (c *Client) DeleteSessionPVC(ctx context.Context, sessionID string) error {
	name := SessionPVCName(sessionID)
	err := c.Core.CoreV1().PersistentVolumeClaims(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		slog.Error("k8s DeleteSessionPVC", "sessionId", sessionID, "pvc", name, "error", err)
		return fmt.Errorf("delete session pvc: %w", err)
	}
	slog.Info("k8s DeleteSessionPVC", "sessionId", sessionID, "pvc", name)
	return nil
}

// cloneInitContainer prepares the session's working tree from the shared clone
// cache, inside the pod, before anything else runs.
//
// `git clone --shared` writes an alternates file pointing back at the cache
// instead of copying objects, so this is near-instant regardless of repo size.
// Its one hazard is handled on the other side: the cache has gc.auto=0, because
// a `git gc` there can prune objects a live session's own commits reference
// (see git.Manager.disableAutoGC).
//
// Re-running on a warm is a no-op — if /workspace already holds a repository,
// the clone is skipped and the existing tree, with whatever uncommitted work is
// in it, is left exactly as it was. Wiping here would destroy work on every
// resume, which is the failure docs/DECISIONS.md records happening twice.
//
// No branch is created. The fleet does not name branches any more; the agent
// runs `git checkout -b` for whatever it decides to call its work.
func cloneInitContainer(image string, spec WorkerPodSpec) corev1.Container {
	base := spec.BaseBranch
	if base == "" {
		base = "main"
	}
	script := fmt.Sprintf(`set -eu
# Git's ownership guard (CVE-2022-24765) refuses every command in a repository
# whose directory belongs to another user. The PVC's mount point is created by
# the kubelet as root and this container runs as the image's non-root user, so
# without this the clone SUCCEEDS and the very next command fails with
# "detected dubious ownership" — which is what happened the first time this
# ran in kind. worker/src/index.ts already carries the same line for the same
# reason; both containers need it because they do not share $HOME.
git config --global --add safe.directory %[1]s
git config --global --add safe.directory /repo-cache/%[2]s

if [ -d %[1]s/.git ]; then
  echo "workspace already has a repository, leaving it alone"
  exit 0
fi
mkdir -p %[1]s
git clone --shared /repo-cache/%[2]s %[1]s
cd %[1]s
git checkout %[3]s 2>/dev/null || git checkout -b %[3]s origin/%[3]s
# The clone's origin points at the local cache, which is useless to the agent:
# it needs to fetch from and push to the real remote.
git remote set-url origin %[4]s
`, sessionWorkdir, spec.Repo, base, spec.RepoURL)

	return corev1.Container{
		Name:    "clone",
		Image:   image,
		Command: []string{"sh", "-c", script},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "session", MountPath: sessionWorkdir, SubPath: "tree"},
			{Name: "shared", MountPath: "/repo-cache", SubPath: "repos", ReadOnly: true},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
}
