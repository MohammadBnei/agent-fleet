package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
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
// never actually implemented until now).
//
// Deliberately NOT under /workspace, and that is a correctness rule rather
// than a preference: /workspace is the git clone the agent is told to commit
// and push. Nested there, a plain `git add -A` sweeps full conversation
// transcripts and the SDK's credentials into a PR, and a `git clean -xfd` — an
// ordinary reset move — destroys the state that makes the session resumable.
// Neither is a mistake the agent could be blamed for; the directory simply
// must not live inside the tree it is being asked to commit.
//
// Per-session isolation comes from the MOUNT (subPath claude-home/<id>), not
// from cwd. It used to come from cwd, when each task had its own worktree path
// and the SDK derived its per-project subdirectory from that path (every
// non-alphanumeric character replaced with `-`, confirmed against the bundled
// cli.js — not a hash). Every session's cwd is /workspace now, so a shared
// root would land all of them in the same `projects/-workspace/`.
const claudeConfigDir = "/claude-home"

// contextBudgetEnv tunes the Agent SDK's own context-management machinery
// for the fleet's usage pattern (ADR-0046). All four are read directly by
// the bundled cli.js; none of them exist as `Options` fields, so env is the
// only way to set them.
//
// The problem they solve: Claude Code's microcompaction — the mechanism
// that silently drops stale tool results mid-session — operates on a
// hardcoded tool set (Read, Bash, Grep, Glob, WebSearch, WebFetch, Edit,
// Write). No MCP tool is in it. ADR-0039 routed every build/test/install
// through `run_command`, an MCP tool, so the fleet's single largest
// context consumer is precisely the one microcompaction never touches. A
// local Claude Code session sheds build output automatically; a worker
// accumulates it until auto-compact summarizes the whole conversation away.
func contextBudgetEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		// Per-result ceiling for MCP tools. The CLI's default is 25000
		// tokens (~100KB), which is bounded but far too generous when a
		// single verbose test run can hit it and then stay in context
		// permanently. 10000 still fits a real stack trace.
		{Name: "MAX_MCP_OUTPUT_TOKENS", Value: "10000"},
		// Server-side context editing (clear_tool_uses_20250919, beta
		// context-management-2025-06-27). USE_API_CLEAR_TOOL_USES rather
		// than USE_API_CLEAR_TOOL_RESULTS *specifically*: this variant
		// sends exclude_tools=[Edit, Write, NotebookEdit], so everything
		// not on that list — including MCP tools — becomes clearable. The
		// other variant sends clear_tool_inputs naming only built-ins and
		// would miss run_command entirely, i.e. it would do nothing for
		// the exact problem this is here to fix.
		//
		// Unverified under subscription OAuth: the beta may be rejected
		// for non-API-key auth. Failure is silent and harmless (the
		// request proceeds without the edit), which is why the other
		// layers here don't depend on it. Confirm via the sawtooth in the
		// worker's `inputTokens` log field, not by assuming.
		{Name: "USE_API_CLEAR_TOOL_USES", Value: "1"},
		// Trigger clearing at 120k input tokens instead of the CLI's 180k
		// default, and clear back down toward 40k. Fleet sessions are
		// long-lived and resumable, so they reach the ceiling far more
		// often than an interactive session does.
		{Name: "API_MAX_INPUT_TOKENS", Value: "120000"},
		{Name: "API_TARGET_INPUT_TOKENS", Value: "40000"},
	}
}

func int32Ptr(i int32) *int32 { return &i }

// CreatePod and its e2e-preview machinery used to fill most of this file:
// a bare long-running Pod, its Service, its IngressRoute, its stripPrefix
// Middleware, its per-task NetworkPolicy, a readiness probe on the app port
// and a resource envelope pinned against the namespace LimitRange.
//
// All of it is deleted (docs/adr/0048 §6). A session has one pod, and what it
// publishes is one Service and one route created on demand by expose().
//
// CreateWorkerPod builds that pod — worker + sidecar (docs/adr/0020 point 5),
// plus the clone init container — wrapped in a batch/v1.Job
// (reliability-findings.md #11) rather than a bare Pod, so terminal-state GC
// comes from Job semantics instead of a hand-rolled sweep. BackoffLimit is
// deliberately 0: a crashed session is a human's decision to warm again, and a
// k8s-level retry would restart an agent mid-conversation with no one asking.
//
// WorkerPodSpec is what a session's pod needs to exist. A struct rather than
// ten positional parameters, which is what this was — and which is how
// `worktreePath` sat in the middle of the list right up until the volume it
// pointed at stopped existing.
type WorkerPodSpec struct {
	SessionID string
	Repo      string
	// RepoURL/BaseBranch are for the clone init container, not for this
	// process: the working tree is cloned inside the pod now (docs/adr/0048
	// §5), because it lives on a volume the provisioner never mounts.
	RepoURL       string
	BaseBranch    string
	LeaseID       string
	ResumeID      string
	ResumeFromSeq int64
	ToolKeys      []string
	ServiceRefs   []ServiceIngredientRef
	ExtraEnv      []corev1.EnvVar
}

func (c *Client) CreateWorkerPod(ctx context.Context, spec WorkerPodSpec) error {
	taskID, repo := spec.SessionID, spec.Repo
	leaseID, resumeSessionID, resumeFromSeq := spec.LeaseID, spec.ResumeID, spec.ResumeFromSeq
	toolKeys, serviceIngredients, extraEnv := spec.ToolKeys, spec.ServiceRefs, spec.ExtraEnv

	name := WorkerResourceName(taskID)
	labels := WorkerLabels(taskID, repo)

	// The session's own working volume, created before the Job so the Job can
	// reference it. local-path + WaitForFirstConsumer, and the pinning is the
	// feature rather than a cost (docs/adr/0048 §4): the volume's node
	// affinity forces every later warm of this session back to the node that
	// already holds its tree and its warm dependency cache, with no affinity
	// rules to write and no re-clone.
	if err := c.EnsureSessionPVC(ctx, taskID, repo); err != nil {
		return fmt.Errorf("ensure session pvc: %w", err)
	}

	sidecarRestartAlways := corev1.ContainerRestartPolicyAlways

	ingredientInitContainers, ingredientEnv, toolsVol, toolsMount, err := buildIngredients(toolKeys, serviceIngredients, ClusterAccess{ExecutorAddr: c.ExecutorAddr, AuthToken: c.ThotAuthToken})
	if err != nil {
		return fmt.Errorf("build ingredients: %w", err)
	}

	workerEnv := []corev1.EnvVar{
		{Name: "SESSION_ID", Value: taskID},
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
		{Name: "WORKTREE_PATH", Value: sessionWorkdir},
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
	workerEnv = append(workerEnv, contextBudgetEnv()...)
	workerEnv = append(workerEnv, ingredientEnv...)
	workerEnv = append(workerEnv, extraEnv...)

	// Four mounts, split by access pattern rather than by uniformity
	// (docs/adr/0048 §4). The old single whole-PVC mount put everything —
	// working tree, node_modules, SDK state — on one Longhorn RWX volume
	// measured at 10 MB/s, where a cold `bun install` could not finish in
	// three minutes. The same install takes 2.4 seconds on node-local disk.
	workerMounts := []corev1.VolumeMount{
		// Node-local, per session, durable across warms.
		{Name: "session", MountPath: "/workspace", SubPath: "tree"},
		// Dependency caches. Same volume, so same node-local speed, and warm
		// across every warm of this session. Deliberately NOT shared between
		// sessions: a per-node volume is not a shape Kubernetes offers, and
		// sharing was never where the measured win came from.
		{Name: "session", MountPath: "/cache", SubPath: "cache"},
		// The clone cache, read-only. Small and read-mostly, so the network
		// volume's latency does not matter — and read-only is a real boundary
		// for the first time: a session cannot corrupt the cache every other
		// session clones from.
		{Name: "shared", MountPath: "/repo-cache", SubPath: "repos", ReadOnly: true},
		// SDK resume state. On replicated storage because losing it loses the
		// ability to resume the conversation at all, which is the one thing
		// here that git is not already a backup of.
		{Name: "shared", MountPath: claudeConfigDir, SubPath: "claude-home/" + taskID},
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
			// Ordered first, and it must stay first: a plain init container
			// runs to completion before the native sidecar below it starts,
			// so the working tree exists before anything tries to read it.
			//
			// This is the only place both volumes are mounted at once, which
			// is why the clone happens here rather than in the provisioner
			// (docs/adr/0048 §5). It also means the clone runs on the node
			// that owns the volume — node-local, not across the 10 MB/s
			// fabric the provisioner would have crossed.
			cloneInitContainer(c.WorkerImage, spec),
			{
				Name:          "sidecar",
				Image:         c.SidecarImage,
				RestartPolicy: &sidecarRestartAlways,
				Env: []corev1.EnvVar{
					{Name: "SESSION_ID", Value: taskID},
					{Name: "TARGET_REPO", Value: repo},
					{Name: "MCP_PORT", Value: fmt.Sprint(SidecarMCPPort)},
					{Name: "LOCAL_API_PORT", Value: fmt.Sprint(SidecarAPIPort)},
					{Name: "WORKTREE_PATH", Value: sessionWorkdir},
					{Name: "LOG_LEVEL", Value: c.LogLevel},
					// Without this, the sidecar falls back to its own
					// separately-hardcoded default, which only happens to
					// match prod's release-prefixed core Service name — it
					// silently doesn't resolve against kind-local's
					// unprefixed one (confirmed live: sidecar stuck at
					// /readyz 503 forever, worker never starts).
					{Name: "CORE_GRPC_ADDR", Value: c.CoreGRPCAddr},
					// FLEET_ENDPOINTS is gone with the roster it carried
					// (docs/adr/0048 §6). It told the sidecar where this
					// session's sandbox would answer, so the first run_command
					// could dial it directly rather than relaying through
					// core. There is no sandbox and no run_command; the
					// sidecar's only outbound connection is the one to core
					// it dials from CORE_GRPC_ADDR above.
					// Deliberately no THOT_* here. ADR-0035's ask_thot is
					// gone, and the executor token now belongs only to the
					// worker container of a cluster-access task (see
					// buildIngredients). Putting it back on every sidecar
					// would hand the cluster credential to tasks that have
					// no business holding it.
					//
					// Git's ownership guard (CVE-2022-24765) — the same one the
					// clone init container disables for itself. This container
					// has no $HOME to write a gitconfig into and runs as a
					// different user than the one that owns the tree, so every
					// telemetry `git diff` would refuse with "detected dubious
					// ownership". That failure surfaces only as permanently-zero
					// diff stats, never as an error anyone sees.
					{Name: "GIT_CONFIG_COUNT", Value: "1"},
					{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
					{Name: "GIT_CONFIG_VALUE_0", Value: "*"},
				},
				Ports: []corev1.ContainerPort{
					{Name: "mcp", ContainerPort: SidecarMCPPort},
					{Name: "local-api", ContainerPort: SidecarAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					// The session's working tree, for the telemetry loop's
					// `git diff` — the sidecar needs to see the same files
					// the agent is editing.
					//
					// A SubPath is fine here now, where it was actively
					// wrong before: the old layout mounted the whole PVC
					// because a LINKED WORKTREE's .git is an absolute-path
					// gitlink back to repos/<repo>/.git/worktrees/<id>, so
					// scoping the mount severed it and every git command
					// answered "not a git repository". There are no linked
					// worktrees any more — this is a real clone with its
					// own .git directory (docs/adr/0048 §5).
					{Name: "session", MountPath: sessionWorkdir, SubPath: "tree"},
					// ...but a `git clone --shared` does not COPY objects:
					// .git/objects/info/alternates points back at the cache,
					// so without this mount `git diff --numstat HEAD` cannot
					// read the HEAD tree and the telemetry loop reports
					// nothing for every session. Seeing the files is not the
					// same as being able to run git on them.
					{Name: "shared", MountPath: "/repo-cache", SubPath: "repos", ReadOnly: true},
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
				Name: "session",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: SessionPVCName(taskID)},
				},
			},
			{
				Name: "shared",
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
		slog.Error("k8s CreateWorkerPod", "sessionId", taskID, "repo", repo, "error", err)
		return err
	}
	slog.Info("k8s CreateWorkerPod", "sessionId", taskID, "repo", repo)
	metrics.PodsCreatedTotal.WithLabelValues("worker").Inc()
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
		slog.Error("k8s DeleteWorkerJob", "sessionId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteWorkerJob", "sessionId", taskID)
	metrics.PodsDeletedTotal.WithLabelValues("worker").Inc()
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
		slog.Error("k8s GetWorkerJobRepo", "sessionId", taskID, "error", err)
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
