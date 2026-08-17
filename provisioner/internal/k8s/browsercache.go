package k8s

import (
	"context"
	"fmt"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BrowserCacheJobName is fixed, not per-session: there is one browser cache on
// the shared PVC and one Job that fills it.
const BrowserCacheJobName = "agent-fleet-browser-cache"

// browserCacheScript installs both browser builds docs/adr/0044 requires into
// the shared PVC.
//
// BOTH, and this is the whole of that ADR: @playwright/mcp bundles a different
// playwright-core than `playwright` does, so it resolves a different build
// number and refuses to start on the one the other install put down. Browser
// automation was dead for the fleet's entire history behind that. The two
// commands used to sit in worker/Dockerfile and have to stay together wherever
// they live.
//
// Idempotent by construction: playwright skips a build that is already
// present, so a re-run against a populated directory downloads nothing and
// exits 0 in seconds. That is what makes it safe to fire on every provisioner
// start rather than tracking a marker — and it means an image bump that
// changes a build number repairs the cache on the next start instead of
// silently serving a browser the new playwright-core will not launch.
//
// Both installers are the image's own pinned binaries, NOT `bunx`. `bunx
// @playwright/mcp install-browser` resolved the package from npm at Job
// runtime — "Resolving dependencies / Saved lockfile" in the live log — so the
// cache got whatever build *latest* happened to want while every worker runs
// the image's pinned @playwright/mcp. That is the exact build-number mismatch
// this two-install script exists to prevent. The versions belong in
// worker/Dockerfile and nowhere else; naming the binaries is what keeps them
// there.
//
// World-readable so a worker image running as some other uid can still read the
// cache. NOT `chmod -R o+rX /browsers`: the mount root is created by kubelet
// and owned by root, this Job runs as uid 1000 (worker/Dockerfile ends
// `USER bun`), and chmod on a directory you do not own is EPERM whatever group
// fsGroup puts you in — two live KubeJobFailed alerts, 2026-08-16 and
// 2026-08-17, both dying here *after* the full download. The root already has
// o+rx from kubelet's own 0775, so only the contents ever needed it.
const browserCacheScript = `set -eu
export PLAYWRIGHT_BROWSERS_PATH=/browsers
playwright install chromium
playwright-mcp install-browser chrome-for-testing
chmod -R o+rX /browsers/*
echo "browser cache ready:"
ls /browsers
`

// EnsureBrowserCache makes sure the shared PVC holds Playwright's browser
// builds, which worker pods mount read-only at PLAYWRIGHT_BROWSERS_PATH
// (see browsersDir in pod.go).
//
// Called at provisioner startup, best-effort: a fleet whose browser cache is
// still filling is a fleet that cannot browse yet, which is strictly better
// than a provisioner that refuses to start. Every other capability works
// meanwhile.
//
// Deliberately a Job on the worker image rather than work the provisioner does
// itself. The provisioner is a Go/debian image with no bun and no playwright,
// and the versions that decide which browser builds are correct live in the
// worker image's node_modules — resolving them anywhere else is how the two
// drift apart.
//
// A healthy existing Job is left alone rather than recreated: Jobs are
// immutable in the fields that matter here, and a completed one is the normal
// steady state. It is deleted and recreated when it references a different
// image — exactly the case where the cache may need new builds — or when it
// has failed.
//
// A failed one has to be retried, because nothing else will. This used to check
// the image alone and never read .status, so a Job that exhausted its
// BackoffLimit was indistinguishable from a healthy completed one: every
// restart logged "already present for this image" and did nothing, leaving a
// dead cache and a KubeJobFailed alert that only a WORKER_IMAGE bump or a
// human `kubectl delete job` could clear (live, 2026-08-17). The cost is that a
// provisioner crash-looping against a genuinely broken script re-downloads each
// time; a permanently dead cache is worse.
func (c *Client) EnsureBrowserCache(ctx context.Context) error {
	jobs := c.Core.BatchV1().Jobs(c.Namespace)

	existing, err := jobs.Get(ctx, BrowserCacheJobName, metav1.GetOptions{})
	if err == nil {
		switch {
		case len(existing.Spec.Template.Spec.Containers) == 0 ||
			existing.Spec.Template.Spec.Containers[0].Image != c.WorkerImage:
			slog.Info("browser cache job is for a different image, recreating",
				"job", BrowserCacheJobName, "want", c.WorkerImage)
		case jobFailed(existing):
			slog.Warn("browser cache job failed, recreating",
				"job", BrowserCacheJobName, "image", c.WorkerImage)
		default:
			slog.Info("browser cache job already present for this image",
				"job", BrowserCacheJobName, "image", c.WorkerImage)
			return nil
		}
		policy := metav1.DeletePropagationBackground
		if err := jobs.Delete(ctx, BrowserCacheJobName, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete stale browser cache job: %w", err)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get browser cache job: %w", err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BrowserCacheJobName,
			Namespace: c.Namespace,
			Labels:    map[string]string{ComponentLabel: "browser-cache"},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{ComponentLabel: "browser-cache"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// Same fsGroup as worker pods (pod.go): kubelet chgrps the
					// subPath to gid 1000 and adds g+w, which is what lets this
					// Job write a mount root it does not own. Without it the
					// subPath stays root-owned 0755 on a real StorageClass and
					// nothing can be installed into it at all.
					//
					// It does NOT make the Job able to chmod that root — fsGroup
					// changes the group, never the owner, and chmod needs
					// ownership. Believing otherwise is what shipped the failed
					// fix on 2026-08-16; see the script comment above.
					SecurityContext: &corev1.PodSecurityContext{FSGroup: int64Ptr(1000)},
					NodeSelector:    c.SessionNodeSelector,
					Containers: []corev1.Container{{
						Name:    "install",
						Image:   c.WorkerImage,
						Command: []string{"sh", "-c", browserCacheScript},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "shared", MountPath: "/browsers", SubPath: browsersSubPath},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2000m"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "shared",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: c.WorkspacePVC,
							},
						},
					}},
				},
			},
		},
	}

	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create browser cache job: %w", err)
	}
	slog.Info("browser cache job created", "job", BrowserCacheJobName, "image", c.WorkerImage)
	return nil
}

// jobFailed reports whether a Job has given up — BackoffLimitExceeded or
// DeadlineExceeded both land here. Not the same as "has a failed pod": a Job
// under its BackoffLimit is still retrying and must be left alone.
func jobFailed(j *batchv1.Job) bool {
	for _, cond := range j.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
