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
// World-readable because the cache is written by this Job (root) and read by
// every worker's `bun` user (uid 1000) through a read-only mount.
const browserCacheScript = `set -eu
export PLAYWRIGHT_BROWSERS_PATH=/browsers
bunx playwright install chromium
bunx @playwright/mcp install-browser chrome-for-testing
chmod -R o+rX /browsers
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
// An existing Job is left alone rather than recreated: Jobs are immutable in
// the fields that matter here, and a completed one is the normal steady state.
// It is deleted and recreated only when it references a different image, which
// is exactly the case where the cache may need new builds.
func (c *Client) EnsureBrowserCache(ctx context.Context) error {
	jobs := c.Core.BatchV1().Jobs(c.Namespace)

	existing, err := jobs.Get(ctx, BrowserCacheJobName, metav1.GetOptions{})
	if err == nil {
		if len(existing.Spec.Template.Spec.Containers) > 0 &&
			existing.Spec.Template.Spec.Containers[0].Image == c.WorkerImage {
			slog.Info("browser cache job already present for this image",
				"job", BrowserCacheJobName, "image", c.WorkerImage)
			return nil
		}
		slog.Info("browser cache job is for a different image, recreating",
			"job", BrowserCacheJobName, "want", c.WorkerImage)
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
					NodeSelector:  c.SessionNodeSelector,
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
