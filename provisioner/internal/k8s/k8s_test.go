package k8s

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestClient() *Client {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		middlewareGVR:   "MiddlewareList",
		ingressRouteGVR: "IngressRouteList",
	}
	return &Client{
		Core:                  fake.NewSimpleClientset(),
		Dynamic:               dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		Namespace:             "agent-fleet",
		RunnerImage:           "mohammaddocker/agent-fleet-e2e-runner:latest",
		WorkerImage:           "mohammaddocker/agent-fleet-worker:latest",
		SidecarImage:          "mohammaddocker/agent-fleet-sidecar:latest",
		WorkspacePVC:          "agent-fleet-workspace",
		PostgresImage:         "postgres:16-alpine",
		RedisImage:            "redis:7-alpine",
		SharedInstancePVCSize: "2Gi",
	}
}

// TestCreatePod_EmptyStartCmdIsASandboxOnlyPod is the inverse of the old
// TestCreatePod_EmptyStartCmd_FailsLoud (docs/adr/0044). An empty start_cmd
// used to be a hard error, which meant every repo without an "e2e" profile —
// agent-fleet and infra-bootstrap among them — could never start a sandbox at
// all, even though run_command is registered for every session from turn one
// (docs/adr/0039). Empty now means "no app", not "no pod".
//
// The readiness probe deliberately stays on AppPort even here: nothing will
// ever bind it, so the pod stays NotReady, and NotReady is the honest answer
// for a sandbox with no app. publishNotReadyAddresses keeps exec/code-server
// routable regardless, and app_ready=false is exactly what the dashboard card
// should show.
func TestCreatePod_EmptyStartCmdIsASandboxOnlyPod(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreatePod(ctx, TaskRef{ID: "task-1", Repo: "dream-analyst"}); err != nil {
		t.Fatalf("a sandbox-only pod must be created, got %v", err)
	}

	pod, err := c.Core.CoreV1().Pods(c.Namespace).Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	container := pod.Spec.Containers[0]
	var startCmd *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "E2E_START_CMD" {
			startCmd = &container.Env[i]
		}
	}
	if startCmd == nil {
		t.Fatal("E2E_START_CMD must still be present so GetPod can read it back")
	}
	if startCmd.Value != "" {
		t.Errorf("expected an empty E2E_START_CMD, got %q", startCmd.Value)
	}
	if container.ReadinessProbe == nil {
		t.Error("the readiness probe must stay unconditional — it is what keeps app_ready honest")
	}
}

// TestCreatePod_MemoryLimitAboveNamespaceDefault is a regression test for
// a bug caught live on the real cluster: the e2e-runner container ran with
// no explicit resources, silently inheriting the agent-fleet namespace's
// LimitRange default (512Mi) — nowhere near enough for code-server +
// headless-Chromium Playwright + the target app's own dev server running
// concurrently, and the pod got OOMKilled mid-run. Must be set explicitly,
// well above that 512Mi default.
func TestCreatePod_MemoryLimitAboveNamespaceDefault(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	task := TaskRef{ID: "mem-check", Repo: "dream-analyst", StartCmd: "bun run dev"}

	if err := c.CreatePod(ctx, task); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName(task.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	container := pod.Spec.Containers[0]
	limit := container.Resources.Limits[corev1.ResourceMemory]
	namespaceDefault := resource.MustParse("512Mi")
	if limit.Cmp(namespaceDefault) <= 0 {
		t.Errorf("e2e-runner memory limit = %s, want something above the namespace LimitRange default of %s", limit.String(), namespaceDefault.String())
	}
}

func TestCreatePod_ShapeMatchesTSVersion(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	task := TaskRef{ID: "abc-123-def", Repo: "dream-analyst", StartCmd: "bun run dev"}

	if err := c.CreatePod(ctx, task); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName(task.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[ComponentLabel] != "e2e-runner" || pod.Labels[TaskIDLabel] != task.ID {
		t.Errorf("unexpected labels: %+v", pod.Labels)
	}
	if pod.Spec.RestartPolicy != "Never" {
		t.Errorf("expected RestartPolicy Never, got %s", pod.Spec.RestartPolicy)
	}
	container := pod.Spec.Containers[0]
	if len(container.VolumeMounts) != 4 {
		t.Errorf("expected 4 volume mounts, got %d: %+v", len(container.VolumeMounts), container.VolumeMounts)
	}
	if container.VolumeMounts[0].SubPath != "worktrees/"+task.ID {
		t.Errorf("unexpected worktree mount: %+v", container.VolumeMounts[0])
	}
	cacheMount := container.VolumeMounts[1]
	if cacheMount.MountPath != "/cache" || cacheMount.SubPath != "cache/"+task.Repo {
		t.Errorf("unexpected cache volume mount: %+v", cacheMount)
	}
	sshAuthKeysMount := container.VolumeMounts[3]
	if sshAuthKeysMount.MountPath != "/ssh-authorized-keys" || !sshAuthKeysMount.ReadOnly {
		t.Errorf("unexpected ssh-authorized-keys mount: %+v", sshAuthKeysMount)
	}
	if len(container.Ports) != 5 {
		t.Errorf("expected 5 ports, got %d: %+v", len(container.Ports), container.Ports)
	}
	if container.Ports[0].ContainerPort != AppPort || container.Ports[1].ContainerPort != CodeServerPort || container.Ports[2].ContainerPort != PlaywrightPort {
		t.Errorf("unexpected first 3 ports: %+v", container.Ports[:3])
	}
	if container.Ports[4].ContainerPort != SSHPort || container.Ports[4].Name != "ssh" {
		t.Errorf("unexpected ssh port: %+v", container.Ports[4])
	}
	dshmVol := pod.Spec.Volumes[1]
	if dshmVol.EmptyDir == nil || dshmVol.EmptyDir.Medium != "Memory" || dshmVol.EmptyDir.SizeLimit.String() != "1Gi" {
		t.Errorf("unexpected dshm volume: %+v", dshmVol)
	}
	wantCacheEnv := map[string]string{
		"GOMODCACHE":            "/cache/go-mod",
		"GOCACHE":               "/cache/go-build",
		"BUN_INSTALL_CACHE_DIR": "/cache/bun",
	}
	gotCacheEnv := map[string]string{}
	for _, e := range container.Env {
		if _, ok := wantCacheEnv[e.Name]; ok {
			gotCacheEnv[e.Name] = e.Value
		}
	}
	for name, want := range wantCacheEnv {
		if gotCacheEnv[name] != want {
			t.Errorf("env %s = %q, want %q", name, gotCacheEnv[name], want)
		}
	}
}

func TestCreateService_Shape(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateService(ctx, "task-1"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := c.Core.CoreV1().Services("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Selector[TaskIDLabel] != "task-1" {
		t.Errorf("unexpected selector: %+v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 5 {
		t.Errorf("expected 5 ports, got %d", len(svc.Spec.Ports))
	}
	sshPort := svc.Spec.Ports[4]
	if sshPort.Port != SSHPort || sshPort.Name != "ssh" {
		t.Errorf("unexpected ssh port: %+v", sshPort)
	}
}

// TestCreateService_PublishesNotReadyAddresses guards the fix that makes the
// e2e pod usable as the worker's sandbox (docs/adr/0039). The pod's
// ReadinessProbe watches AppPort only, so ready-gated endpoints took exec,
// playwright and code-server down with the target app — leaving no way in
// at the one moment anyone needs one, and silently defeating docs/adr/0036's
// stated reason for choosing readiness over liveness.
func TestCreateService_PublishesNotReadyAddresses(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateService(ctx, "task-1"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := c.Core.CoreV1().Services("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("publishNotReadyAddresses must be true — otherwise run_command and code-server are unreachable whenever the app isn't listening on AppPort")
	}
}

// TestCreateService_SelectorExcludesWorkerPod is a regression test for a bug
// caught live on the real cluster: the selector was TaskIDLabel alone, and
// WorkerLabels carries that very same label — so the task's worker pod
// joined this Service's EndpointSlice alongside the e2e pod, and roughly
// half of Traefik's preview requests were load-balanced onto a pod with
// nothing listening on AppPort. Symptom was an intermittent 502 that looked
// exactly like a broken app.
func TestCreateService_SelectorExcludesWorkerPod(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateService(ctx, "task-1"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := c.Core.CoreV1().Services("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	matches := func(podLabels map[string]string) bool {
		for k, v := range svc.Spec.Selector {
			if podLabels[k] != v {
				return false
			}
		}
		return true
	}
	if !matches(Labels("task-1")) {
		t.Errorf("selector %+v must match the e2e pod's own labels", svc.Spec.Selector)
	}
	if matches(WorkerLabels("task-1", "dream-analyst")) {
		t.Errorf("selector %+v also matches the worker pod for the same task — it must not", svc.Spec.Selector)
	}
}

// TestCreatePod_ReadinessProbeOnAppPort guards the other half of that same
// live 502: with no probe, an app that binds the wrong port or only
// 127.0.0.1 (Vite's default, reproduced live) leaves the pod reporting
// Running forever while the preview never serves. Readiness specifically —
// a startup or liveness probe would kill the pod and take code-server with
// it, removing the only way to debug the failure.
func TestCreatePod_ReadinessProbeOnAppPort(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	task := TaskRef{ID: "probe-check", Repo: "dream-analyst", StartCmd: "bun run dev"}
	if err := c.CreatePod(ctx, task); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	pod, err := c.Core.CoreV1().Pods("agent-fleet").Get(ctx, ResourceName(task.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	container := pod.Spec.Containers[0]
	probe := container.ReadinessProbe
	if probe == nil || probe.TCPSocket == nil || probe.TCPSocket.Port.IntVal != AppPort {
		t.Fatalf("expected a TCP readiness probe on AppPort, got %+v", probe)
	}
	if container.StartupProbe != nil || container.LivenessProbe != nil {
		t.Error("e2e-runner must not have a startup/liveness probe — a failing app must not kill code-server")
	}
	// A cold `bun install` measured 782s live; the window must comfortably
	// exceed that or every cold cache reports a false failure.
	if window := probe.PeriodSeconds * probe.FailureThreshold; window < 900 {
		t.Errorf("readiness window = %ds, want >= 900s to cover a cold dependency install", window)
	}
}

// Only /code is stripped now. The app is served at the root of its own
// hostname (docs/adr/0038), so stripping anything from it is exactly the bug
// this replaced — an app emitting /assets/... 404'd because it was never told
// its public base path.
func TestCreateMiddleware_StripPrefixPaths(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateMiddleware(ctx, "task-1"); err != nil {
		t.Fatalf("CreateMiddleware: %v", err)
	}
	obj, err := c.Dynamic.Resource(middlewareGVR).Namespace("agent-fleet").Get(ctx, stripPrefixName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get middleware: %v", err)
	}
	prefixes, _, _ := unstructuredNestedSlice(obj.Object, "spec", "stripPrefix", "prefixes")
	if len(prefixes) != 1 || prefixes[0] != "/code" {
		t.Errorf("expected exactly [/code], got %+v", prefixes)
	}
}

// PreviewURLFor is the single string the whole per-task-subdomain fix hinges
// on: root path, own hostname, no prefix anywhere.
func TestPreviewURLFor_RootPathSubdomain(t *testing.T) {
	got := PreviewURLFor("e2e.bnei.dev", "task-1")
	if want := "https://task1.e2e.bnei.dev/"; got != want {
		t.Errorf("PreviewURLFor = %q, want %q", got, want)
	}
	if strings.Contains(got, "/app") {
		t.Errorf("PreviewURLFor still carries a path prefix: %q", got)
	}
}

func TestCreateIngressRoute_Routes(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateIngressRoute(ctx, "e2e.bnei.dev", "task-1"); err != nil {
		t.Fatalf("CreateIngressRoute: %v", err)
	}
	obj, err := c.Dynamic.Resource(ingressRouteGVR).Namespace("agent-fleet").Get(ctx, ResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingressroute: %v", err)
	}

	// The wildcard must be declared explicitly, or Traefik derives the
	// concrete per-task hostname from the Host() rule and orders one cert per
	// session — against a 50-per-registered-domain-per-week budget shared with
	// every other bnei.dev host.
	resolver, _, _ := unstructuredNestedString(obj.Object, "spec", "tls", "certResolver")
	if resolver != "le-dns" {
		t.Errorf("certResolver = %q, want le-dns (TLS-ALPN-01 cannot issue a wildcard)", resolver)
	}
	domains, _, _ := unstructuredNestedSlice(obj.Object, "spec", "tls", "domains")
	if len(domains) != 1 || domains[0].(map[string]any)["main"] != "*.e2e.bnei.dev" {
		t.Fatalf("expected a single *.e2e.bnei.dev wildcard, got %+v", domains)
	}

	routes, _, _ := unstructuredNestedSlice(obj.Object, "spec", "routes")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}

	code := routes[0].(map[string]any)
	if code["match"] != "Host(`task1.e2e.bnei.dev`) && PathPrefix(`/code`)" {
		t.Errorf("unexpected code-server match: %v", code["match"])
	}

	app := routes[1].(map[string]any)
	if app["match"] != "Host(`task1.e2e.bnei.dev`)" {
		t.Errorf("unexpected app match: %v", app["match"])
	}
	// The load-bearing assertion: no stripPrefix on the app route. Adding one
	// back reintroduces the base-path bug for every target app.
	for _, mw := range app["middlewares"].([]any) {
		if name := mw.(map[string]any)["name"]; name == stripPrefixName("task-1") {
			t.Errorf("app route must NOT carry stripPrefix — it is served at the root")
		}
	}
	// code-server must outrank the app route, else a target app owning /code
	// wins on a rule-length tiebreak.
	if code["priority"].(int64) <= app["priority"].(int64) {
		t.Errorf("code route priority %v must exceed app route %v", code["priority"], app["priority"])
	}
}

func TestDeleteAll_IgnoresNotFound(t *testing.T) {
	c := newTestClient()
	if err := c.DeleteAll(context.Background(), "never-existed"); err != nil {
		t.Fatalf("DeleteAll should ignore 404s, got: %v", err)
	}
}

func TestCreateWorkerPod_TwoContainersSharedPVC(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "lease-1", "/workspace/worktrees/task-1", "", 0, nil, nil, nil); err != nil {
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
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Name != "sidecar" {
		t.Fatalf("expected 1 init container (sidecar), got %+v", podSpec.InitContainers)
	}
	sidecar := podSpec.InitContainers[0]
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("sidecar init container must be a native sidecar (RestartPolicy: Always), got %v", sidecar.RestartPolicy)
	}
	if sidecar.StartupProbe == nil || sidecar.StartupProbe.HTTPGet == nil || sidecar.StartupProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("sidecar init container must have an HTTP /readyz startup probe so the worker waits for a proven core connection, got %+v", sidecar.StartupProbe)
	}
	// Whole PVC, no SubPath: a linked git worktree's .git gitlink is an
	// absolute path back to repos/<repo>/.git/worktrees/<taskId>, which a
	// SubPath scoped to just worktrees/<taskId> would put out of reach.
	for _, ctr := range append(append([]corev1.Container{}, podSpec.Containers...), podSpec.InitContainers...) {
		if len(ctr.VolumeMounts) != 1 || ctr.VolumeMounts[0].SubPath != "" || ctr.VolumeMounts[0].MountPath != "/workspace" {
			t.Errorf("container %s: unexpected volume mounts: %+v", ctr.Name, ctr.VolumeMounts)
		}
		foundWorktreePath := false
		for _, e := range ctr.Env {
			if e.Name == "WORKTREE_PATH" {
				foundWorktreePath = true
				if e.Value != "/workspace/worktrees/task-1" {
					t.Errorf("container %s: unexpected WORKTREE_PATH: %q", ctr.Name, e.Value)
				}
			}
		}
		if !foundWorktreePath {
			t.Errorf("container %s: missing WORKTREE_PATH env var", ctr.Name)
		}
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].PersistentVolumeClaim.ClaimName != "agent-fleet-workspace" {
		t.Errorf("expected single shared-PVC volume, got: %+v", podSpec.Volumes)
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

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "lease-1", "/workspace/worktrees/task-1", "sess-abc123", 42, nil, nil, nil); err != nil {
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
	if err := c.CreateWorkerPod(ctx, "task-2", "dream-analyst", "lease-2", "/workspace/worktrees/task-2", "", 0, nil, nil, nil); err != nil {
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

func TestGetPod_ExistsVsNotFound(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	_, exists, err := c.GetPod(ctx, "never-existed")
	if err != nil || exists {
		t.Fatalf("expected exists=false for a missing pod, got exists=%v err=%v", exists, err)
	}

	// GetPod is the e2e-preview-pod getter (worker sessions are Jobs now,
	// reliability-findings.md #11) — exercised against CreatePod, not
	// CreateWorkerPod.
	if err := c.CreatePod(ctx, TaskRef{ID: "task-1", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	_, exists, err = c.GetPod(ctx, ResourceName("task-1"))
	if err != nil || !exists {
		t.Fatalf("expected exists=true for a created pod, got exists=%v err=%v", exists, err)
	}
}

func TestGetWorkerJobRepo_RecoversRepoFromLabel(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "vos-monolith", "lease-1", "/workspace/worktrees/task-1", "", 0, nil, nil, nil); err != nil {
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

func TestListWorkerJobsByLabel_ExcludesE2ePods(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	if err := c.CreateWorkerPod(ctx, "task-1", "dream-analyst", "lease-1", "/workspace/worktrees/task-1", "", 0, nil, nil, nil); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if err := c.CreatePod(ctx, TaskRef{ID: "task-2", Repo: "dream-analyst", StartCmd: "bun run dev"}); err != nil {
		t.Fatalf("CreatePod (e2e): %v", err)
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
	cases := []struct {
		name  string
		conds []batchv1.JobCondition
		want  string
	}{
		{"no conditions yet", nil, ""},
		{"complete", []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}}, "Succeeded"},
		{"failed", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}}, "Failed"},
		{"condition present but not True", []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "False"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tc.conds}}
			if got := jobPhase(job); got != tc.want {
				t.Errorf("jobPhase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// small helper avoiding an extra import for one nested-slice read
func unstructuredNestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	cur := obj
	for i, f := range fields {
		if i == len(fields)-1 {
			v, ok := cur[f].([]any)
			return v, ok, nil
		}
		next, ok := cur[f].(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur = next
	}
	return nil, false, nil
}

// same, for a nested string read
func unstructuredNestedString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := obj
	for i, f := range fields {
		if i == len(fields)-1 {
			v, ok := cur[f].(string)
			return v, ok, nil
		}
		next, ok := cur[f].(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = next
	}
	return "", false, nil
}

// The inverse of the guard this replaces. ADR-0035 injected the executor
// bearer token into every sidecar so ask_thot could register; ADR-0037
// deleted ask_thot but the injection survived the deletion, leaving every
// ordinary repo task holding a cluster credential it cannot use and must
// not have. The token now belongs only to the worker container of a task
// that actually declares cluster-access.
func TestCreateWorkerPod_SidecarCarriesNoClusterCredential(t *testing.T) {
	c := newTestClient()
	c.ThotAuthToken = "test-token"

	if err := c.CreateWorkerPod(context.Background(), "task-1", "dream-analyst", "lease-1", "/workspace/worktrees/task-1", "", 0, nil, nil, nil); err != nil {
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

	if err := c.CreateWorkerPod(context.Background(), "task-thot", "infra-bootstrap", "lease-1",
		"/workspace/worktrees/task-thot", "", 0, []string{"cluster-access"}, nil, nil); err != nil {
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
