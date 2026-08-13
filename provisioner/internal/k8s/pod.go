package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// workerJobTTLSeconds is how long a finished worker Job (and the pod it
// owns) sticks around before Kubernetes' TTL controller garbage-collects
// it — reliability-findings.md #11: this replaces the reconcile loop's own
// hand-rolled terminal-phase GC pass. Long enough to `kubectl logs` a
// crashed worker, short enough not to accumulate.
const workerJobTTLSeconds = 300

// claudeConfigDir redirects the Agent SDK's session-state directory
// (normally $HOME/.claude, which is a fresh container filesystem every
// pod) onto the shared workspace PVC — without this, `resume:` has
// nothing to resume from regardless of what session id is passed
// (sessions redesign, supersedes docs/adr/0021/0025's phase-boundary
// framing; the redirect itself was originally described in ADR-0016 but
// never actually implemented until now). One shared root is safe across
// concurrent tasks: the SDK's own per-project subdirectory is derived
// directly from `cwd` (every non-alphanumeric character replaced with
// `-`, confirmed against the bundled SDK's cli.js — not a hash), and
// every task's cwd is already a distinct worktree path.
const claudeConfigDir = "/workspace/.claude-home"

// e2eMaxCPU/e2eMaxMemory are the e2e-runner container's limits. They must
// stay <= limitRange.max in k8s/core.yaml — that LimitRange is
// namespace-wide, so exceeding it means the pod is rejected at admission
// and never exists, rather than merely being throttled. Named constants so
// the test that pins the two files together has something to assert on.
const (
	e2eMaxCPU    = "4000m"
	e2eMaxMemory = "4Gi"
)

func int32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool    { return &b }

// CreatedE2ePod mirrors CreatedE2ePod in k8s.ts.
type CreatedE2ePod struct {
	PodName     string
	IngressPath string
}

// TaskRef is the subset of a task row this package needs.
type TaskRef struct {
	ID   string
	Repo string
	// StartCmd is always resolved by the caller now (core resolves the
	// repo's "e2e" profile, or an agent-supplied override — docs/adr/0034)
	// — the old per-repo StartCmdFor Go-switch fallback is gone, verified
	// working end-to-end via /kind-local before removal.
	//
	// Empty is VALID (docs/adr/0044): it means a sandbox-only pod — no app,
	// no preview, run_command fully working. It used to be a hard error
	// here, which made every repo without an "e2e" profile (agent-fleet,
	// infra-bootstrap) unable to start a sandbox at all, even though
	// run_command is registered for every session from turn one.
	StartCmd string
	// ToolKeys/ServiceIngredients are the repo's resolved "e2e" profile
	// (docs/adr/0034) — core resolves the profile name, this package only
	// ever sees the already-resolved ingredient list. ExtraEnv carries
	// already-minted task-scoped/repo-scoped service URLs (the caller,
	// grpcserver.go, calls MintServiceCredentials before CreatePod so a
	// minting failure blocks pod creation entirely — pod-scoped ingredients
	// are the only kind actually materialized inside this function, via
	// buildIngredients).
	ToolKeys           []string
	ServiceIngredients []ServiceIngredientRef
	ExtraEnv           []corev1.EnvVar
}

func (c *Client) CreatePod(ctx context.Context, task TaskRef) error {
	startCmd := task.StartCmd
	name := ResourceName(task.ID)
	labels := Labels(task.ID)

	initContainers, ingredientEnv, toolsVol, toolsMount, err := buildIngredients(task.ToolKeys, task.ServiceIngredients, ClusterAccess{ExecutorAddr: c.ExecutorAddr, AuthToken: c.ThotAuthToken})
	if err != nil {
		return fmt.Errorf("build ingredients: %w", err)
	}

	runnerEnv := []corev1.EnvVar{
		{Name: "E2E_WORKTREE_PATH", Value: "/workspace"},
		{Name: "E2E_START_CMD", Value: startCmd},
		{Name: "E2E_APP_PORT", Value: fmt.Sprint(AppPort)},
		{Name: "E2E_CODE_SERVER_PORT", Value: fmt.Sprint(CodeServerPort)},
		{Name: "E2E_PLAYWRIGHT_PORT", Value: fmt.Sprint(PlaywrightPort)},
		{Name: "E2E_EXEC_PORT", Value: fmt.Sprint(ExecPort)},
		{Name: "E2E_SSH_PORT", Value: fmt.Sprint(SSHPort)},
		// Persisted on the shared PVC under cache/<repo>/ (see the
		// "cache" VolumeMount below) so a repo's Go/Bun deps only get
		// downloaded once, not on every e2e session — GOCACHE/GOMODCACHE
		// and BUN_INSTALL_CACHE_DIR are real env vars both toolchains
		// already read natively, and both create the directory on first
		// write, so no init container is needed. E2E_START_CMD inherits
		// these as container env, same as every other var here.
		// ponytail: no fleet-level lock — two concurrent e2e pods for the
		// same repo share this dir relying on Go/Bun's own cache-safe
		// locking; add a mutex (mirroring internal/git's per-repo one) if
		// that ever proves flaky in practice.
		{Name: "GOMODCACHE", Value: "/cache/go-mod"},
		{Name: "GOCACHE", Value: "/cache/go-build"},
		{Name: "BUN_INSTALL_CACHE_DIR", Value: "/cache/bun"},
	}
	runnerEnv = append(runnerEnv, ingredientEnv...)
	runnerEnv = append(runnerEnv, task.ExtraEnv...)

	runnerMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/workspace", SubPath: "worktrees/" + task.ID},
		{Name: "workspace", MountPath: "/cache", SubPath: "cache/" + task.Repo},
		{Name: "dshm", MountPath: "/dev/shm"},
		{Name: "ssh-authorized-keys", MountPath: "/ssh-authorized-keys", ReadOnly: true},
	}
	if toolsMount != nil {
		runnerMounts = append(runnerMounts, *toolsMount)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			Containers: []corev1.Container{
				{
					Name:  "e2e-runner",
					Image: c.RunnerImage,
					Env:   runnerEnv,
					Ports: []corev1.ContainerPort{
						{Name: "app", ContainerPort: AppPort},
						{Name: "code-server", ContainerPort: CodeServerPort},
						{Name: "playwright", ContainerPort: PlaywrightPort},
						{Name: "exec", ContainerPort: ExecPort},
						{Name: "ssh", ContainerPort: SSHPort},
					},
					VolumeMounts: runnerMounts,
					// Readiness, deliberately not startup/liveness: a pod
					// whose app never binds must stay alive so code-server
					// (:8080) is still reachable to debug why. This is the
					// only thing standing between "the recipe's start_cmd
					// is wrong" and a preview that 502s forever while the
					// pod reports Running — found live: Vite bound
					// 127.0.0.1:5173 and nothing anywhere noticed.
					// FailureThreshold is huge on purpose: a cold
					// `bun install` on the shared cache measured 782s.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(AppPort)}},
						PeriodSeconds:    10,
						FailureThreshold: 120,
					},
					// Explicit, not left to the namespace LimitRange's
					// 512Mi container default (agent-fleet-core-limits,
					// common-app-chart) — found live via OOMKilled: this
					// one container runs code-server, a headless-Chromium
					// Playwright MCP server, and the target app's own dev
					// server (a real npm/bun install + Vite) all at once,
					// nowhere near fitting in 512Mi.
					//
					// Raised from 250m/512Mi req, 2000m/2Gi lim once this
					// pod became the worker's build/test sandbox
					// (docs/adr/0039) rather than just a preview: it now
					// runs the compiles and test suites too, concurrently
					// with all of the above. The old 250m request was the
					// bigger problem of the two — it sets the CFS weight,
					// so under any node contention the pod got a quarter
					// core to install dependencies with.
					//
					// The 4000m ceiling REQUIRES limitRange.max.cpu >= 4 in
					// k8s/core.yaml. That LimitRange is namespace-wide
					// (created by core's release, applied to pods the
					// provisioner creates), its chart default is cpu "2",
					// and a container limit above the max is rejected at
					// admission — the pod is never created at all. Change
					// both or neither; TestCreatePod_ResourcesWithinLimitRange
					// pins them together.
					//
					// Sizing note: the fleet concurrency cap is 5, so 5
					// concurrent sandboxes reserve 5 CPU / 5Gi against
					// worker-01+worker-02's 12 cores — the 4-core limit is
					// burst headroom for the common idle-node case, not a
					// reservation.
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1000m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(e2eMaxCPU),
							corev1.ResourceMemory: resource.MustParse(e2eMaxMemory),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
					},
				},
				{
					Name: "dshm",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: resourcePtr("1Gi"),
						},
					},
				},
				{
					Name: "ssh-authorized-keys",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName:  "e2e-ssh-authorized-keys",
							Optional:    boolPtr(true),
							Items:       []corev1.KeyToPath{{Key: "E2E_SSH_AUTHORIZED_KEYS", Path: "authorized_keys"}},
							DefaultMode: int32Ptr(0644),
						},
					},
				},
			},
		},
	}
	if toolsVol != nil {
		pod.Spec.Volumes = append(pod.Spec.Volumes, *toolsVol)
	}

	if _, err := c.Core.CoreV1().Pods(c.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		slog.Error("k8s CreatePod", "taskId", task.ID, "error", err)
		return err
	}
	slog.Info("k8s CreatePod", "taskId", task.ID)
	return nil
}

func (c *Client) DeletePod(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.CoreV1().Pods(c.Namespace).Delete(ctx, ResourceName(taskID), metav1.DeleteOptions{}))
	if err != nil {
		slog.Error("k8s DeletePod", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeletePod", "taskId", taskID)
	return nil
}

// WaitForPodGone blocks until the named pod is fully deleted, or timeout.
// Needed because pod deletion is asynchronous: the API object survives its
// whole terminationGracePeriod, so "kill_env then request_e2e_env" — the
// single most common agent sequence — would otherwise find the dying pod
// still present and hand back its preview URL.
//
// ponytail: a 1s poll, no informer/watch. The wait is bounded, happens on
// one RPC, and the provisioner already polls elsewhere; a shared informer
// cache is only worth it if this ever runs hot.
func (c *Client) WaitForPodGone(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.Core.CoreV1().Pods(c.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s still terminating after %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// PodState is what an e2e pod can say about itself. Phase alone was the
// whole story until a preview 502'd for 20 minutes while the pod sat at
// Running: AppReady (the AppPort readiness probe) is what distinguishes a
// slow install from an app that bound the wrong interface, and StartCmd is
// read back off the live pod rather than re-resolved from repo_profiles so
// it reflects what is genuinely running, override included.
type PodState struct {
	Phase     corev1.PodPhase
	AppReady  bool
	Restarts  int32
	StartedAt string // RFC3339, empty until scheduled
	StartCmd  string
	// Terminating is the difference between "a session is live" and "a
	// session is on its way out". A pod with a deletionTimestamp still
	// exists and still reports Phase=Running for its whole grace period, so
	// an existence check alone reads a dying pod as a healthy session —
	// CreateE2eSession used to return that pod's preview URL and the caller
	// never learned the pod was about to vanish.
	Terminating bool
	// Detail is the human-readable reason behind a stuck or dead pod —
	// ImagePullBackOff, OOMKilled, an admission rejection. It exists so the
	// log line on docs/adr/0044's Failed-pod recreate says WHY the corpse was
	// replaced; deleting the pod also deletes its `kubectl logs`, so without
	// this the only remaining trace is whatever reached Loki.
	Detail string
}

// GetPod returns the pod's current state, or exists=false if it doesn't —
// Kubernetes itself is this fleet's durable source of truth for "is this
// session active," now that the provisioner holds no DB credentials
// (docs/adr/0020 point 1). A provisioner restart loses nothing: pod
// existence and state survive it for free, no separate in-memory or
// Postgres-backed tracking needed.
func (c *Client) GetPod(ctx context.Context, name string) (state PodState, exists bool, err error) {
	pod, err := c.Core.CoreV1().Pods(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return PodState{}, false, nil
	}
	if err != nil {
		slog.Error("k8s GetPod", "name", name, "error", err)
		return PodState{}, false, err
	}
	state = PodState{Phase: pod.Status.Phase, Terminating: pod.DeletionTimestamp != nil}
	if pod.Status.StartTime != nil {
		state.StartedAt = pod.Status.StartTime.UTC().Format(time.RFC3339)
	}
	// Single-container pod by construction (CreatePod above), so the
	// e2e-runner's own readiness is the pod's readiness.
	if len(pod.Status.ContainerStatuses) > 0 {
		cs := pod.Status.ContainerStatuses[0]
		state.AppReady = cs.Ready
		state.Restarts = cs.RestartCount
		switch {
		case cs.State.Waiting != nil:
			state.Detail = cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		case cs.State.Terminated != nil:
			state.Detail = fmt.Sprintf("%s (exit %d): %s",
				cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}
	if state.Detail == "" {
		state.Detail = pod.Status.Reason
	}
	if len(pod.Spec.Containers) > 0 {
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Name == "E2E_START_CMD" {
				state.StartCmd = e.Value
				break
			}
		}
	}
	return state, true, nil
}

// CreateWorkerPod builds the two-container pod (worker + sidecar,
// docs/adr/0020 point 5) for a claimed task, wrapped in a batch/v1.Job
// (reliability-findings.md #11) rather than a bare Pod — retry/backoff
// and terminal-state GC come from Job's own semantics instead of the
// reconcile loop hand-rolling them. BackoffLimit is deliberately 0: core's
// heartbeat/reclaim (ClaimNextTask) stays the sole retry mechanism
// (ADR-0020 pt.2 — the provisioner never decides pod lifecycle on its
// own), a k8s-level retry here would silently double up against it.
//
// e2e-preview pods (CreatePod, below) deliberately stay bare Pods: they're
// long-running/interactive, killed explicitly via KillE2eSession, not
// run-to-completion — Job's retry/backoff/TTL semantics don't apply to a
// workload with no expected completion, and (per reconcile/loop.go's own
// doc comment) they were never part of the hand-rolled GC this finding is
// about in the first place.
// toolKeys/serviceIngredients are the repo's resolved "worker" profile
// (docs/adr/0034, empty when the repo has no such profile — preserves
// today's exact pod shape). extraEnv carries already-minted task-scoped/
// repo-scoped service URLs, computed by the caller before this call so a
// minting failure blocks pod creation entirely (grpcserver.go).
func (c *Client) CreateWorkerPod(ctx context.Context, taskID, repo, leaseID, worktreePath, resumeSessionID string, resumeFromSeq int64, toolKeys []string, serviceIngredients []ServiceIngredientRef, extraEnv []corev1.EnvVar) error {
	name := WorkerResourceName(taskID)
	labels := WorkerLabels(taskID, repo)

	sidecarRestartAlways := corev1.ContainerRestartPolicyAlways

	ingredientInitContainers, ingredientEnv, toolsVol, toolsMount, err := buildIngredients(toolKeys, serviceIngredients, ClusterAccess{ExecutorAddr: c.ExecutorAddr, AuthToken: c.ThotAuthToken})
	if err != nil {
		return fmt.Errorf("build ingredients: %w", err)
	}

	workerEnv := []corev1.EnvVar{
		{Name: "TASK_ID", Value: taskID},
		{Name: "TARGET_REPO", Value: repo},
		{Name: "LEASE_ID", Value: leaseID},
		{Name: "SIDECAR_MCP_ADDR", Value: fmt.Sprintf("localhost:%d", SidecarMCPPort)},
		{Name: "SIDECAR_API_ADDR", Value: fmt.Sprintf("localhost:%d", SidecarAPIPort)},
		// The worker's own git push/gh pr create needs auth
		// independently of the provisioner's clone/fetch —
		// separate containers, only /workspace is shared, not
		// $HOME (see worker/src/index.ts's configureGitAuth).
		// Forwarded from the provisioner's own Infisical-sourced
		// env, same value.
		{Name: "GH_TOKEN", Value: os.Getenv("GH_TOKEN")},
		// The Agent SDK reads this straight from process.env
		// (worker/src/session.ts) — without forwarding it here,
		// no worker pod can authenticate to run Claude Code at
		// all, since containers don't inherit env from whatever
		// created them.
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")},
		{Name: "CLAUDE_MODEL", Value: os.Getenv("CLAUDE_MODEL")},
		{Name: "MAX_TURNS", Value: os.Getenv("MAX_TURNS")},
		{Name: "WORKTREE_PATH", Value: worktreePath},
		{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigDir},
		{Name: "RESUME_SESSION_ID", Value: resumeSessionID},
		{Name: "RESUME_FROM_SEQ", Value: strconv.FormatInt(resumeFromSeq, 10)},
		{Name: "LOG_LEVEL", Value: c.LogLevel},
		// The Agent SDK emits `tool_progress` (a long-running Bash call's
		// elapsed time) only when it believes it is running remotely or in
		// a container — it checks CLAUDE_CODE_REMOTE/CLAUDE_CODE_CONTAINER_ID
		// and otherwise drops the event before it reaches the message
		// stream. Every worker genuinely is a container, and without this
		// the worker's tool_progress relay is unreachable code: a
		// four-minute Bash call stays indistinguishable from a hung pod.
		// Value is only tested for emptiness by the SDK; the task ID makes
		// it identifiable in a log.
		{Name: "CLAUDE_CODE_CONTAINER_ID", Value: taskID},
	}
	workerEnv = append(workerEnv, ingredientEnv...)
	workerEnv = append(workerEnv, extraEnv...)

	workerMounts := []corev1.VolumeMount{
		// Whole PVC, not a per-task SubPath — see the sidecar
		// container's identical mount above for why.
		{Name: "workspace", MountPath: "/workspace"},
	}
	if toolsMount != nil {
		workerMounts = append(workerMounts, *toolsMount)
	}

	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// sidecar runs as a native sidecar (K8s 1.29+, this cluster is
		// v1.35): an init container with RestartPolicy Always plus a
		// StartupProbe, so kubelet blocks starting the worker container
		// until the sidecar is actually accepting connections. Without
		// this, both containers start concurrently and the worker's
		// first sidecar call can lose the race (observed live: worker
		// crashed 6ms after start, ~7s before the sidecar logged
		// "listening").
		InitContainers: []corev1.Container{
			{
				Name:          "sidecar",
				Image:         c.SidecarImage,
				RestartPolicy: &sidecarRestartAlways,
				Env: []corev1.EnvVar{
					{Name: "TASK_ID", Value: taskID},
					{Name: "TARGET_REPO", Value: repo},
					{Name: "MCP_PORT", Value: fmt.Sprint(SidecarMCPPort)},
					{Name: "LOCAL_API_PORT", Value: fmt.Sprint(SidecarAPIPort)},
					{Name: "WORKTREE_PATH", Value: worktreePath},
					{Name: "LOG_LEVEL", Value: c.LogLevel},
					// Without this, the sidecar falls back to its own
					// separately-hardcoded default, which only happens to
					// match prod's release-prefixed core Service name — it
					// silently doesn't resolve against kind-local's
					// unprefixed one (confirmed live: sidecar stuck at
					// /readyz 503 forever, worker never starts).
					{Name: "CORE_GRPC_ADDR", Value: c.CoreGRPCAddr},
					// Deliberately no THOT_* here. ADR-0035's ask_thot is
					// gone, and the executor token now belongs only to the
					// worker container of a cluster-access task (see
					// buildIngredients). Putting it back on every sidecar
					// would hand the cluster credential to tasks that have
					// no business holding it.
				},
				Ports: []corev1.ContainerPort{
					{Name: "mcp", ContainerPort: SidecarMCPPort},
					{Name: "local-api", ContainerPort: SidecarAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					// Whole PVC, not a per-task SubPath: a linked git
					// worktree's .git file is an absolute-path gitlink
					// back to the main clone's repos/<repo>/.git/worktrees/
					// <taskId> admin dir (HEAD/index/commondir) — a
					// SubPath scoped to just worktrees/<taskId> cuts that
					// path off entirely, so every git command in this
					// container failed with "not a git repository"
					// (produced by design in ADR-0019 but never checked
					// against how linked worktrees actually work).
					{Name: "workspace", MountPath: "/workspace"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
				// HTTP on /readyz, not a bare TCP check: a TCP probe only
				// proves the sidecar process is listening, not that it can
				// actually reach core — a sidecar with zero core
				// connectivity passed a TCP probe every time, unblocking
				// the worker container against a core it couldn't talk to
				// (observed live: worker crashed 6ms after start via an
				// unguarded first sidecar call, during a core rollout
				// blip). /readyz only returns 200 once the sidecar's
				// coreclient has an actual live connection. Budget widened
				// from the prior ~30s to ~2min to ride out a core rollout,
				// not just a process-start race.
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/readyz",
							Port: intstr.FromInt32(SidecarAPIPort),
						},
					},
					PeriodSeconds:    2,
					FailureThreshold: 60,
				},
			},
		},
		Containers: []corev1.Container{
			{
				Name:         "worker",
				Image:        c.WorkerImage,
				Env:          workerEnv,
				VolumeMounts: workerMounts,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2000m"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
				},
			},
		},
	}
	podSpec.InitContainers = append(podSpec.InitContainers, ingredientInitContainers...)
	if toolsVol != nil {
		podSpec.Volumes = append(podSpec.Volumes, *toolsVol)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(workerJobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}

	_, err = c.Core.BatchV1().Jobs(c.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		slog.Error("k8s CreateWorkerPod", "taskId", taskID, "repo", repo, "error", err)
		return err
	}
	slog.Info("k8s CreateWorkerPod", "taskId", taskID, "repo", repo)
	return nil
}

// jobForegroundDeletion cascades to the Job's own Pod synchronously —
// without it, the default background propagation can leave the Pod
// visible for a moment after DeleteWorkerJob returns.
func jobForegroundDeletion() metav1.DeleteOptions {
	policy := metav1.DeletePropagationForeground
	return metav1.DeleteOptions{PropagationPolicy: &policy}
}

func (c *Client) DeleteWorkerJob(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.BatchV1().Jobs(c.Namespace).Delete(ctx, WorkerResourceName(taskID), jobForegroundDeletion()))
	if err != nil {
		slog.Error("k8s DeleteWorkerJob", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteWorkerJob", "taskId", taskID)
	return nil
}

// GetWorkerJobRepo recovers which repo a worker job belongs to from its
// own RepoLabel — the only way to know, since the provisioner holds no DB
// credentials to look it up any other way (docs/adr/0020 point 1). Needed
// by TearDownSession to remove the right worktree.
func (c *Client) GetWorkerJobRepo(ctx context.Context, taskID string) (repo string, exists bool, err error) {
	job, err := c.Core.BatchV1().Jobs(c.Namespace).Get(ctx, WorkerResourceName(taskID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		slog.Error("k8s GetWorkerJobRepo", "taskId", taskID, "error", err)
		return "", false, err
	}
	return job.Labels[RepoLabel], true, nil
}

func resourcePtr(qty string) *resource.Quantity {
	q := resource.MustParse(qty)
	return &q
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ignoreAlreadyExists makes the e2e session's non-pod resources
// create-if-absent. All three (Service, Middleware, IngressRoute) are
// deterministic from the task ID alone, so an existing one is already the
// one we would have created. Needed because a pod can go away on its own —
// eviction, node drain, OOM — leaving its siblings behind; without this,
// the next request_e2e_env dies on "services \"e2e-<id>\" already exists"
// after having successfully recreated the pod, leaving a half-built
// session. Caught by TestCreateE2ESession_TerminatingPodIsNotAnExistingSession.
//
// ponytail: no reconcile-to-match. If a provisioner upgrade ever changes a
// port or a route, a session created before it keeps the old shape until
// it's torn down — switch to a server-side apply if that stops being fine.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
