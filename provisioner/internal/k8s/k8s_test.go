package k8s

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient() *Client {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		ingressRouteGVR: "IngressRouteList",
	}
	return &Client{
		Core:                  fake.NewSimpleClientset(),
		Dynamic:               dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		Namespace:             "agent-fleet",
		WorkerImage:           "mohammaddocker/agent-fleet-worker:latest",
		SidecarImage:          "mohammaddocker/agent-fleet-sidecar:latest",
		WorkspacePVC:          "agent-fleet-workspace",
		PostgresImage:         "postgres:16-alpine",
		RedisImage:            "redis:7-alpine",
		SharedInstancePVCSize: "2Gi",
	}
}

func TestCreateWorkerPod_TwoContainersSharedPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Labels[ComponentLabel] != ComponentWorker || job.Labels[RepoLabel] != "dream-analyst" {
		t.Errorf("unexpected labels: %+v", job.Labels)
	}
	// reliability-findings.md #11: core's heartbeat/reclaim stays the sole
	// retry mechanism — a k8s-level retry here would double up against it.
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("expected BackoffLimit 0 (no k8s-level retry), got %v", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != workerJobTTLSeconds {
		t.Errorf("expected TTLSecondsAfterFinished %d, got %v", workerJobTTLSeconds, job.Spec.TTLSecondsAfterFinished)
	}
	podSpec := job.Spec.Template.Spec
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Name != "worker" {
		t.Fatalf("expected 1 container (worker), got %+v", podSpec.Containers)
	}
	// clone THEN sidecar, and the order is load-bearing: a plain init
	// container runs to completion before a native sidecar starts, so the
	// working tree exists before anything reads it. Reversing these would
	// start the sidecar — and therefore unblock the worker — against an empty
	// /workspace (docs/adr/0048 §5).
	if len(podSpec.InitContainers) != 2 {
		t.Fatalf("expected 2 init containers (clone, sidecar), got %+v", podSpec.InitContainers)
	}
	if podSpec.InitContainers[0].Name != "clone" {
		t.Fatalf("clone must run first, got %q", podSpec.InitContainers[0].Name)
	}
	if podSpec.InitContainers[1].Name != "sidecar" {
		t.Fatalf("expected sidecar second, got %q", podSpec.InitContainers[1].Name)
	}
	// The clone container is the only place both volumes are mounted, which is
	// the whole reason the clone happens in the pod rather than in the
	// provisioner.
	clone := podSpec.InitContainers[0]
	var sawTree, sawCache bool
	for _, m := range clone.VolumeMounts {
		if m.MountPath == "/workspace" && m.Name == "session" {
			sawTree = true
		}
		if m.MountPath == "/repo-cache" && m.Name == "shared" && m.ReadOnly {
			sawCache = true
		}
	}
	if !sawTree || !sawCache {
		t.Fatalf("clone container must mount the session volume rw and the repo cache ro, got %+v", clone.VolumeMounts)
	}
	// The wildcard specifically, not a list of paths. safe.directory compares
	// exactly against the path git names, and a local-transport clone names the
	// cache's .git — so listing /repo-cache/<repo> matched nothing and every
	// clone died with "Could not read from remote repository".
	if !strings.Contains(strings.Join(clone.Command, "\n"), "safe.directory '*'") {
		t.Errorf("clone container must wildcard safe.directory, got %q", clone.Command)
	}
	sidecar := podSpec.InitContainers[1]
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sidecar init container must be a native sidecar (RestartPolicy: Always), got %v", sidecar.RestartPolicy)
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.HTTPGet == nil || sidecar.StartupProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("sidecar init container must have an HTTP /readyz startup probe so the worker waits for a proven core connection, got %+v", sidecar.StartupProbe)
	}
	// Storage split by access pattern (docs/adr/0048 §4), replacing the single
	// whole-PVC mount every container used to share.
	//
	// The old shape put the working tree, node_modules and the SDK state all
	// on one Longhorn RWX volume measured at 10 MB/s, where a cold
	// `bun install` could not finish in three minutes. The same install takes
	// 2.4 seconds on node-local disk. The four mounts below are that
	// measurement expressed as a pod spec.
	worker := podSpec.Containers[0]
	want := map[string]struct {
		vol      string
		subPath  string
		readOnly bool
	}{
		// Node-local, per session, and the working directory itself.
		"/workspace": {vol: "session", subPath: "tree"},
		// Dependency caches: same node-local volume, so the measured speed
		// applies, and warm across every warm of this session.
		"/cache": {vol: "session", subPath: "cache"},
		// The clone cache every session clones from — read-only, so one
		// session cannot corrupt it for the others.
		"/repo-cache": {vol: "shared", subPath: "repos", readOnly: true},
		// SDK resume state, on replicated storage because losing it loses the
		// ability to continue the conversation at all.
		claudeConfigDir: {vol: "shared", subPath: "claude-home/task-1"},
	}
	got := map[string]corev1.VolumeMount{}
	for _, m := range worker.VolumeMounts {
		got[m.MountPath] = m
	}
	for path, w := range want {
		m, ok := got[path]
		if !ok {
			t.Errorf("worker is missing the %s mount", path)
			continue
		}
		if m.Name != w.vol || m.SubPath != w.subPath || m.ReadOnly != w.readOnly {
			t.Errorf("worker mount %s: got volume=%q subPath=%q readOnly=%v, want volume=%q subPath=%q readOnly=%v",
				path, m.Name, m.SubPath, m.ReadOnly, w.vol, w.subPath, w.readOnly)
		}
	}

	// Every session's cwd is now the same literal path, which is exactly why
	// claude-home has to be per-session: the SDK derives its project state
	// directory from cwd, so without the per-session mount every session would
	// share one `projects/-workspace/` and replay each other's conversations.
	for _, ctr := range []corev1.Container{worker, podSpec.InitContainers[1]} {
		for _, e := range ctr.Env {
			if e.Name == "WORKTREE_PATH" && e.Value != "/workspace" {
				t.Errorf("container %s: WORKTREE_PATH should be the fixed session workdir, got %q", ctr.Name, e.Value)
			}
		}
	}

	if len(podSpec.Volumes) != 2 {
		t.Fatalf("expected two volumes (session, shared), got: %+v", podSpec.Volumes)
	}
	vols := map[string]string{}
	for _, v := range podSpec.Volumes {
		vols[v.Name] = v.PersistentVolumeClaim.ClaimName
	}
	if vols["session"] != SessionPVCName("task-1") {
		t.Errorf("session volume should be this session's own PVC, got %q", vols["session"])
	}
	// Every mount, in every container, must name a volume this pod declares.
	//
	// The fake clientset does not validate this — that check lives in the real
	// API server — so renaming a volume and missing one mount produced a spec
	// that passed every unit test here and was then rejected outright at
	// creation: `initContainers[1].volumeMounts[0].name: Not found:
	// "workspace"`. No Job, no pod, and the only evidence was one provisioner
	// log line. Found in kind; this is the assertion that makes it a test
	// failure instead.
	declared := map[string]bool{}
	for _, v := range podSpec.Volumes {
		declared[v.Name] = true
	}
	all := append(append([]corev1.Container{}, podSpec.Containers...), podSpec.InitContainers...)
	for _, ctr := range all {
		for _, m := range ctr.VolumeMounts {
			if !declared[m.Name] {
				t.Errorf("container %s mounts volume %q, which the pod does not declare — "+
					"the API server rejects the whole Job for this", ctr.Name, m.Name)
			}
		}
	}

	if vols["shared"] != "agent-fleet-workspace" {
		t.Errorf("shared volume should be the fleet-wide PVC, got: %+v", podSpec.Volumes)
	}
}

// findEnv returns the value of the first env var named `name`, and whether
// it was found at all — RESUME_SESSION_ID legitimately has an empty-string
// value for a brand-new task, so "found but empty" and "not set" have to
// be distinguishable.
func findEnv(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// TestCreateWorkerPod_ResumeSession covers the sessions redesign's resume
// path (supersedes docs/adr/0021/0025's phase-boundary framing, completes
// the CLAUDE_CONFIG_DIR redirect ADR-0016 described but never wired up) —
// CLAUDE_CONFIG_DIR must always point at the shared PVC (or `resume:` has
// nothing to resume from regardless of what session id is passed), and
// RESUME_SESSION_ID must carry through whatever resumeSessionID was given,
// including empty for a fresh task.
func TestCreateWorkerPod_ResumeSession(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "sess-abc123", ResumeFromSeq: 42, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	if configDir, ok := findEnv(worker.Env, "CLAUDE_CONFIG_DIR"); !ok || configDir != claudeConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q (found=%v), want %q", configDir, ok, claudeConfigDir)
	}
	if resumeID, ok := findEnv(worker.Env, "RESUME_SESSION_ID"); !ok || resumeID != "sess-abc123" {
		t.Errorf("RESUME_SESSION_ID = %q (found=%v), want %q", resumeID, ok, "sess-abc123")
	}
	if resumeFromSeq, ok := findEnv(worker.Env, "RESUME_FROM_SEQ"); !ok || resumeFromSeq != "42" {
		t.Errorf("RESUME_FROM_SEQ = %q (found=%v), want %q", resumeFromSeq, ok, "42")
	}
	// Non-empty is the whole contract: the Agent SDK drops tool_progress
	// events entirely unless it thinks it is containerized, which makes the
	// worker's relay for them unreachable. Verified against the SDK bundle's
	// own `if(!process.env.CLAUDE_CODE_REMOTE&&!process.env.CLAUDE_CODE_CONTAINER_ID)break`.
	if containerID, ok := findEnv(worker.Env, "CLAUDE_CODE_CONTAINER_ID"); !ok || containerID == "" {
		t.Errorf("CLAUDE_CODE_CONTAINER_ID = %q (found=%v), want non-empty", containerID, ok)
	}

	// A fresh task (no prior session) must still set RESUME_SESSION_ID —
	// present-but-empty, not omitted, so worker/src/session.ts's env read
	// doesn't have to distinguish "unset" from "empty" itself.
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-2", Repo: "dream-analyst", LeaseID: "lease-2", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job2, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-2"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if resumeID, ok := findEnv(job2.Spec.Template.Spec.Containers[0].Env, "RESUME_SESSION_ID"); !ok || resumeID != "" {
		t.Errorf("fresh task RESUME_SESSION_ID = %q (found=%v), want empty-but-present", resumeID, ok)
	}
	if resumeFromSeq, ok := findEnv(job2.Spec.Template.Spec.Containers[0].Env, "RESUME_FROM_SEQ"); !ok || resumeFromSeq != "0" {
		t.Errorf("fresh task RESUME_FROM_SEQ = %q (found=%v), want %q", resumeFromSeq, ok, "0")
	}
}

func TestGetWorkerJobRepo_RecoversRepoFromLabel(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "vos-monolith", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	repo, exists, err := c.GetWorkerJobRepo(ctx, "task-1")
	if err != nil || !exists {
		t.Fatalf("expected exists=true, got exists=%v err=%v", exists, err)
	}
	if repo != "vos-monolith" {
		t.Errorf("expected repo=vos-monolith, got %q", repo)
	}

	_, exists, err = c.GetWorkerJobRepo(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing job, got exists=%v err=%v", exists, err)
	}
}

// This used to also create an e2e pod and assert it was excluded from the
// listing. There are no e2e pods (docs/adr/0048 §6), so what is left to pin is
// that the selector is a selector at all — it must match worker Jobs by label
// rather than returning everything in the namespace, since core reconciles
// every session's pod_phase against this answer and a listing that over- or
// under-reports frees or holds concurrency slots wrongly.
func TestListWorkerJobsByLabel_SelectsWorkerJobs(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	jobs, err := c.ListWorkerJobsByLabel(ctx)
	if err != nil {
		t.Fatalf("ListWorkerJobsByLabel: %v", err)
	}
	if len(jobs) != 1 || jobs[0].TaskID != "task-1" {
		t.Errorf("expected only the worker job listed, got %+v", jobs)
	}
}

func TestJobPhase_DerivedFromConditions(t *testing.T) {
	ready := func(n int32) *int32 { return &n }
	cases := []struct {
		name  string
		conds []batchv1.JobCondition
		ready *int32
		want  string
	}{
		{"no conditions yet", nil, nil, ""},
		{"complete", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}, nil, "Succeeded"},
		{"failed", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}}, nil, "Failed"},
		{"condition present but not True", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "False"}}, nil, ""},
		// The string core maps onto POD_PHASE_RUNNING. Without it the upward
		// sync is unreachable and every healthy session sits at SCHEDULED for
		// its whole life — which is what the fleet did until 2026-08-15,
		// while core's test for that sync passed against a fake that returned
		// "Running" the real thing never emitted.
		{"pod ready", nil, ready(1), "Running"},
		// Still pulling/scheduling: Active would be 1 here, which is why the
		// check is on Ready.
		{"pod not ready yet", nil, ready(0), ""},
		// A finished Job whose last pod still reports ready must not read as
		// live — terminal wins.
		{"complete outranks ready", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}, ready(1), "Succeeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conds, Ready: tc.ready}}
			if got := jobPhase(job); got != tc.want {
				t.Errorf("jobPhase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// small helper avoiding an extra import for one nested-slice read
// The inverse of the guard this replaces. ADR-0035 injected the executor
// bearer token into every sidecar so ask_thot could register; ADR-0037
// deleted ask_thot but the injection survived the deletion, leaving every
// ordinary repo task holding a cluster credential it cannot use and must
// not have. The token now belongs only to the worker container of a task
// that actually declares cluster-access.
func TestCreateWorkerPod_SidecarCarriesNoClusterCredential(t *testing.T) {
	c := newTestClient()
	c.ThotAuthToken = "test-token"

	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "dream-analyst", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: nil, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	jobs, err := c.Core.BatchV1().Jobs("agent-fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d (err=%v)", len(jobs.Items), err)
	}

	// Native sidecar: an init container with restartPolicy Always
	// (needs K8s >= 1.29), not a regular container.
	var sidecar *corev1.Container
	for i, ct := range jobs.Items[0].Spec.Template.Spec.InitContainers {
		if ct.Name == "sidecar" {
			sidecar = &jobs.Items[0].Spec.Template.Spec.InitContainers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("no sidecar container in the worker Job")
	}

	for _, e := range sidecar.Env {
		if strings.HasPrefix(e.Name, "THOT_") || e.Name == "EXECUTOR_ADDR" {
			t.Errorf("sidecar carries %s=%q; cluster credentials belong only to a cluster-access worker container", e.Name, e.Value)
		}
	}
}

// TestCreateWorkerPod_ClusterAccessReachesTheJob proves the cluster-access
// ingredient survives all the way into the real Job spec, not just
// buildIngredients' return value — the wiring between them is where a
// silently-missing capability would hide (docs/adr/0037).
func TestCreateWorkerPod_ClusterAccessReachesTheJob(t *testing.T) {
	c := newTestClient()
	c.ExecutorAddr = "thot-executor.thot.svc.cluster.local:9090"
	c.ThotAuthToken = "tok"

	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-thot", Repo: "infra-bootstrap", LeaseID: "lease-1", ResumeID: "", ResumeFromSeq: 0, ToolKeys: []string{"cluster-access"}, ServiceRefs: nil, ExtraEnv: nil}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	jobs, err := c.Core.BatchV1().Jobs("agent-fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d (err=%v)", len(jobs.Items), err)
	}
	spec := jobs.Items[0].Spec.Template.Spec

	var staged bool
	for _, ic := range spec.InitContainers {
		if ic.Name == "tool-cluster-access" {
			staged = true
		}
	}
	if !staged {
		t.Error("no tool-cluster-access init container — the kubectl shim is never staged")
	}

	// The worker container is what runs the agent's Bash, so that's where
	// the shim's env has to land.
	var worker *corev1.Container
	for i, ct := range spec.Containers {
		if ct.Name == "worker" {
			worker = &spec.Containers[i]
		}
	}
	if worker == nil {
		t.Fatal("no worker container")
	}
	got := map[string]string{}
	for _, e := range worker.Env {
		got[e.Name] = e.Value
	}
	if got["EXECUTOR_ADDR"] == "" {
		t.Error("EXECUTOR_ADDR missing from the worker container — kubectl shim exits 2")
	}
	if got["THOT_AUTH_TOKEN"] == "" {
		t.Error("THOT_AUTH_TOKEN missing from the worker container — executor rejects the call")
	}
}

// The browser mount and the env var that points at it are a pair: either
// alone is a silently browser-less session.
//
// This is docs/adr/0044's failure mode with the browsers in a new place. That
// ADR's last of three stacked failures was Playwright looking under
// $HOME/.cache/ms-playwright and reporting the browser as not installed —
// which is exactly what happens if the mount lands and
// PLAYWRIGHT_BROWSERS_PATH does not. The MCP server still starts, the tool
// list still registers, and only a real browser_navigate finds out.
//
// Read-only is asserted too: the cache is 2 GB shared by every session, and a
// writable mount would make one session able to corrupt the browser build all
// the others launch — the shared-writable-volume pattern docs/DECISIONS.md
// forbids.
func TestCreateWorkerPod_MountsTheSharedBrowserCacheReadOnlyAndPointsPlaywrightAtIt(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
		SessionID: "task-browsers", Repo: "dream-analyst", LeaseID: "lease-1",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-browsers"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	worker := job.Spec.Template.Spec.Containers[0]

	var mount *corev1.VolumeMount
	for i, m := range worker.VolumeMounts {
		if m.MountPath == browsersDir {
			mount = &worker.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("worker has no %s mount — every session is browser-less", browsersDir)
	}
	if mount.Name != "shared" || mount.SubPath != browsersSubPath {
		t.Errorf("browser mount = volume %q subPath %q, want shared/%s", mount.Name, mount.SubPath, browsersSubPath)
	}
	if !mount.ReadOnly {
		t.Error("the browser cache is mounted writable — one session can corrupt the browser build every other session launches")
	}

	var browsersPath string
	for _, e := range worker.Env {
		if e.Name == "PLAYWRIGHT_BROWSERS_PATH" {
			browsersPath = e.Value
		}
	}
	if browsersPath != browsersDir {
		t.Errorf("PLAYWRIGHT_BROWSERS_PATH = %q, want %q — without it Playwright looks in $HOME/.cache and reports no browser installed, and the mount above is inert",
			browsersPath, browsersDir)
	}
}

// The cache Job must write the SAME directory the worker pods read, and must
// run the worker image — the playwright/playwright-core versions that decide
// which browser builds are correct live in that image's node_modules, and
// resolving them anywhere else is how the two drift apart (docs/adr/0044).
func TestEnsureBrowserCache_WritesWhatTheWorkersRead(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.EnsureBrowserCache(ctx); err != nil {
		t.Fatalf("EnsureBrowserCache: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, BrowserCacheJobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get browser cache job: %v", err)
	}
	ctr := job.Spec.Template.Spec.Containers[0]
	if ctr.Image != c.WorkerImage {
		t.Errorf("cache job image = %q, want the worker image %q", ctr.Image, c.WorkerImage)
	}
	if len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].SubPath != browsersSubPath {
		t.Fatalf("cache job must write the shared PVC's %q subPath, got %+v", browsersSubPath, ctr.VolumeMounts)
	}
	if ctr.VolumeMounts[0].ReadOnly {
		t.Error("the cache job's own mount is read-only — it cannot populate anything")
	}
	// fsGroup, same as worker pods: without it the subPath stays root-owned
	// 0755 on a real StorageClass and the script's `chmod -R o+rX /browsers`
	// dies with "Operation not permitted" after the browsers download (live
	// KubeJobFailed 2026-08-16).
	if sc := job.Spec.Template.Spec.SecurityContext; sc == nil || sc.FSGroup == nil || *sc.FSGroup != 1000 {
		t.Error("cache job needs fsGroup 1000, else its chmod on the root-owned subPath fails")
	}
	script := strings.Join(ctr.Command, "\n")
	// BOTH installs, which is the entirety of docs/adr/0044: the two packages
	// bundle different playwright-core versions and resolve different build
	// numbers, so one alone leaves the MCP server unable to launch.
	if !strings.Contains(script, "playwright install chromium") {
		t.Error("cache job never installs chromium")
	}
	if !strings.Contains(script, "install-browser chrome-for-testing") {
		t.Error("cache job skips @playwright/mcp's own browser build — docs/adr/0044: the MCP server " +
			"resolves a different build number and refuses to start on the other one")
	}

	// Re-running against the same image must not churn the Job. It is fired on
	// every provisioner start, and a restart loop that recreates a 2 GB
	// download each time is worse than no cache at all.
	if err := c.EnsureBrowserCache(ctx); err != nil {
		t.Fatalf("second EnsureBrowserCache: %v", err)
	}
	jobs, err := c.Core.BatchV1().Jobs("agent-fleet").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, j := range jobs.Items {
		if j.Name == BrowserCacheJobName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d browser cache jobs after two calls, want 1", n)
	}
}

// TestCreateWorkerPod_RepoImageAppliesToTheWorkerContainerOnly guards
// `repos.image`, the column docs/adr/0048 §6 put in place of the four
// toolchain ingredients.
//
// The column, its store round-trip and its dashboard input all shipped with
// that ADR — and the value never left core. There was no `image` field on
// CreateWorkerPodRequest, so pod.go took the provisioner's WORKER_IMAGE
// unconditionally and editing the field in the dashboard did nothing at all.
// Nothing errored, because nothing was individually wrong: every layer was
// correct and none of them was connected to the next.
//
// The second half is the other direction of the same bug. The clone init
// container and the sidecar must stay on the fleet's own images: cloning
// needs git, and the sidecar is a fleet binary at a fixed path. A
// repo-supplied image owes neither. Applying the image to all three is a
// one-token mistake (c.WorkerImage -> workerImage on the wrong line) that no
// build or lint would catch, since all four values are plain strings — the
// same shape as the SaveAgentSessionId swap in CLAUDE.md's traps.
func TestCreateWorkerPod_RepoImageAppliesToTheWorkerContainerOnly(t *testing.T) {
	const repoImage = "registry.example.test/some-repo-toolchain:v9"

	for _, tc := range []struct {
		name      string
		specImage string
		wantImage string
	}{
		{name: "repo sets an image", specImage: repoImage, wantImage: repoImage},
		{name: "empty falls back to the fleet default", specImage: "", wantImage: "mohammaddocker/agent-fleet-worker:latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient()
			ctx := context.Background()

			if err := c.CreateWorkerPod(ctx, WorkerPodSpec{
				SessionID: "task-img", Repo: "dream-analyst", LeaseID: "lease-1", Image: tc.specImage,
			}); err != nil {
				t.Fatalf("CreateWorkerPod: %v", err)
			}

			job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName("task-img"), metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			podSpec := job.Spec.Template.Spec

			if got := podSpec.Containers[0].Image; got != tc.wantImage {
				t.Errorf("worker container image = %q, want %q", got, tc.wantImage)
			}

			byName := map[string]string{}
			for _, ic := range podSpec.InitContainers {
				byName[ic.Name] = ic.Image
			}
			if got := byName["clone"]; got != c.WorkerImage {
				t.Errorf("clone init container image = %q, want the fleet's own %q — it needs git, which a repo image need not have", got, c.WorkerImage)
			}
			if got := byName["sidecar"]; got != c.SidecarImage {
				t.Errorf("sidecar image = %q, want %q", got, c.SidecarImage)
			}
		})
	}
}
