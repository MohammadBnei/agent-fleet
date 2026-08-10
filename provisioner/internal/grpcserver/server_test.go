package grpcserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/mcpproxy"
)

func newFakeK8sClient() *k8s.Client {
	scheme := runtime.NewScheme()
	middlewareGVR := schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}
	ingressRouteGVR := schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	listKinds := map[schema.GroupVersionResource]string{
		middlewareGVR:   "MiddlewareList",
		ingressRouteGVR: "IngressRouteList",
	}
	return &k8s.Client{
		Core:         fake.NewSimpleClientset(),
		Dynamic:      dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		Namespace:    "agent-fleet",
		RunnerImage:  "mohammaddocker/agent-fleet-e2e-runner:latest",
		WorkerImage:  "mohammaddocker/agent-fleet-worker:latest",
		SidecarImage: "mohammaddocker/agent-fleet-sidecar:latest",
		WorkspacePVC: "agent-fleet-workspace",
	}
}

type fakeEventReporter struct {
	events []*agentfleetv1.PodEvent
}

func (f *fakeEventReporter) ReportEvent(ctx context.Context, event *agentfleetv1.PodEvent) {
	f.events = append(f.events, event)
}

// newTestOriginRepo creates a real local git repo (a commit on "main") to
// clone from — same "real temp git repo, no mocking" convention
// worker/src/git.test.ts already used for this exact kind of logic.
func newTestOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

// newTestServer leaves fleetSharedRepoURL empty — SyncFleetShared is
// disabled, so existing tests that don't care about it don't need a real
// git origin. TestCreateWorkerPod_SyncsFleetShared below builds its own
// Server with a real one.
func newTestServer(t *testing.T) (*Server, *k8s.Client, *fakeEventReporter) {
	t.Helper()
	k8sc := newFakeK8sClient()
	gitMgr := git.NewManager(t.TempDir())
	proxy := mcpproxy.New(func(taskID string) string { return "" }, func(taskID string) string { return "" })
	reporter := &fakeEventReporter{}
	return New(k8sc, gitMgr, proxy, reporter, "e2e.bnei.dev", "", "", ""), k8sc, reporter
}

func TestKillE2ESession_NoActiveSession(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp, err := s.KillE2ESession(context.Background(), &agentfleetv1.KillE2ESessionRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("KillE2ESession: %v", err)
	}
	if resp.GetKilled() {
		t.Errorf("expected killed=false when no session exists")
	}
}

func TestCreateE2ESession_Idempotent(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()

	first, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{TaskId: "t1", Repo: "dream-analyst"})
	if err != nil {
		t.Fatalf("first CreateE2ESession: %v", err)
	}
	if first.GetPreviewUrl() == "" {
		t.Fatal("expected a preview URL on first creation")
	}

	second, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{TaskId: "t1", Repo: "dream-analyst"})
	if err != nil {
		t.Fatalf("second CreateE2ESession: %v", err)
	}
	if second.GetPreviewUrl() != first.GetPreviewUrl() {
		t.Errorf("expected the same preview URL on a repeat call, got %q vs %q", second.GetPreviewUrl(), first.GetPreviewUrl())
	}

	pods, err := k8sc.Core.CoreV1().Pods("agent-fleet").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected exactly 1 e2e pod after two CreateE2ESession calls, got %d", len(pods.Items))
	}
}

func TestKillE2ESession_ActiveSession(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := s.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{TaskId: "t1", Repo: "dream-analyst"}); err != nil {
		t.Fatalf("CreateE2ESession: %v", err)
	}
	resp, err := s.KillE2ESession(ctx, &agentfleetv1.KillE2ESessionRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("KillE2ESession: %v", err)
	}
	if !resp.GetKilled() {
		t.Errorf("expected killed=true")
	}
}

func TestCreateWorkerPod_ClonesAndCreatesPod(t *testing.T) {
	s, k8sc, reporter := newTestServer(t)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	resp, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		TaskId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if resp.GetPodName() != k8s.WorkerResourceName("task-1") {
		t.Errorf("unexpected pod name: %s", resp.GetPodName())
	}

	// Worker sessions are batch/v1.Jobs now (reliability-findings.md #11),
	// not bare Pods — GetWorkerJobRepo is the existence check that actually
	// applies here.
	if _, exists, err := k8sc.GetWorkerJobRepo(ctx, "task-1"); err != nil || !exists {
		t.Fatalf("expected the worker job to exist: exists=%v err=%v", exists, err)
	}

	sawScheduled := false
	for _, e := range reporter.events {
		if e.GetPhase() == agentfleetv1.PodPhase_POD_PHASE_SCHEDULED {
			sawScheduled = true
		}
	}
	if !sawScheduled {
		t.Errorf("expected a SCHEDULED pod event to be reported, got %+v", reporter.events)
	}
}

// TestCreateWorkerPod_SyncsFleetShared covers docs/adr/0032's wiring: a
// dispatch with a configured fleetSharedRepoURL must sync it into
// claudeHomeDir before the pod is created — this is the whole reason a
// worker's settingSources: ["user"] later finds anything there.
func TestCreateWorkerPod_SyncsFleetShared(t *testing.T) {
	k8sc := newFakeK8sClient()
	gitMgr := git.NewManager(t.TempDir())
	proxy := mcpproxy.New(func(taskID string) string { return "" }, func(taskID string) string { return "" })
	reporter := &fakeEventReporter{}
	claudeHome := filepath.Join(t.TempDir(), "claude-home")

	fleetSharedOrigin := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = fleetSharedOrigin
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	// fleetSharedRepoURL is a whole monorepo in practice (config's default
	// points at this same repo) — content lives under a fleet-shared/
	// subdirectory, not the clone root (git.Manager.SyncFleetShared descends
	// into it; a flat repo-root fixture masked a real bug caught live via
	// kind-local, see git_test.go's newFleetSharedOriginRepo).
	if err := os.MkdirAll(filepath.Join(fleetSharedOrigin, "fleet-shared"), 0o755); err != nil {
		t.Fatalf("mkdir fleet-shared: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fleetSharedOrigin, "fleet-shared", "CLAUDE.md"), []byte("fleet context"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")

	s := New(k8sc, gitMgr, proxy, reporter, "e2e.bnei.dev", fleetSharedOrigin, "main", claudeHome)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		TaskId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	if b, err := os.ReadFile(filepath.Join(claudeHome, "CLAUDE.md")); err != nil || string(b) != "fleet context" {
		t.Fatalf("expected fleet-shared CLAUDE.md mirrored into claudeHome, got %q err %v", b, err)
	}
}

func TestTearDownSession_Worker_NoopWhenNothingToTearDown(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp, err := s.TearDownSession(context.Background(), &agentfleetv1.TearDownSessionRequest{
		TaskId: "never-existed", Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER,
	})
	if err != nil {
		t.Fatalf("TearDownSession: %v", err)
	}
	if resp.GetTornDown() {
		t.Errorf("expected torn_down=false for a task with no active worker pod")
	}
}

// TestTearDownSession_Worker_RemovesJobButKeepsWorktree is the regression
// guard reliability-findings.md #2 explicitly notes was missing before
// this fix: teardown deletes the Job, but must leave the worktree/branch
// alone — the old unconditional branch -D here destroyed the only
// reference to a never-pushed branch's commits whenever a terminal status
// was reached via a git push failure.
func TestTearDownSession_Worker_RemovesJobButKeepsWorktree(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		TaskId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	resp, err := s.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{
		TaskId: "task-1", Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER,
	})
	if err != nil {
		t.Fatalf("TearDownSession: %v", err)
	}
	if !resp.GetTornDown() {
		t.Errorf("expected torn_down=true")
	}
	if _, exists, err := k8sc.GetWorkerJobRepo(ctx, "task-1"); err != nil || exists {
		t.Fatalf("expected the worker job to be deleted: exists=%v err=%v", exists, err)
	}

	worktrees, err := s.git.ListWorktrees(ctx, "dream-analyst")
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(worktrees) != 1 || worktrees[0].TaskID != "task-1" {
		t.Fatalf("expected task-1's worktree to survive teardown, got %+v", worktrees)
	}
}
