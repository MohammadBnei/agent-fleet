package grpcserver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
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
	reporter := &fakeEventReporter{}
	return New(k8sc, gitMgr, reporter, "e2e.bnei.dev", "", "", ""), k8sc, reporter
}

func TestCreateWorkerPod_ClonesAndCreatesPod(t *testing.T) {
	s, k8sc, reporter := newTestServer(t)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	resp, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
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

	s := New(k8sc, gitMgr, reporter, "e2e.bnei.dev", fleetSharedOrigin, "main", claudeHome)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	// Into THIS SESSION's claude-home, not one directory shared by every pod.
	//
	// The old layout rsync'd --delete over settings.json and skills/ in a
	// single shared directory on every dispatch, while other sessions were
	// mid-turn, with no lock (docs/adr/0048 §4). Per-session seeding removes
	// that race by construction: it is written once, before this pod exists,
	// and nothing rewrites it while the session is live.
	sessionHome := gitMgr.ClaudeHomePath("task-1")
	if b, err := os.ReadFile(filepath.Join(sessionHome, "CLAUDE.md")); err != nil || string(b) != "fleet context" {
		t.Fatalf("expected fleet-shared CLAUDE.md mirrored into this session's claude-home (%s), got %q err %v", sessionHome, b, err)
	}

	// And nothing was written to the shared root that every session used to
	// share — if this file appears, the per-session mount is decorative and
	// two sessions can still overwrite each other's settings.
	if _, err := os.Stat(filepath.Join(claudeHome, "CLAUDE.md")); err == nil {
		t.Error("fleet-shared was seeded into the OLD shared claude-home as well — " +
			"sessions would still be writing over each other")
	}
}

func TestTearDownSession_Worker_NoopWhenNothingToTearDown(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp, err := s.TearDownSession(context.Background(), &agentfleetv1.TearDownSessionRequest{
		SessionId: "never-existed", Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER,
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
		SessionId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}

	resp, err := s.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{
		SessionId: "task-1", Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER,
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

	// The session's disk survives teardown — that is what makes a stopped
	// session resumable rather than merely remembered. Only the retention GC
	// reclaims it, and only through SweepSession.
	//
	// This used to check a worktree. There are none now; a session's tree is
	// its own PVC (docs/adr/0048 §4), so this asserts the PVC is still there.
	pvcName := k8s.SessionPVCName("task-1")
	if _, err := k8sc.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, pvcName, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected task-1's working volume to survive teardown, got %v", err)
	}

}

// SweepSession is the one thing that DOES reclaim it, and it must take both
// halves — they live on different volumes, so deleting one and forgetting the
// other leaks silently and forever.
func TestSweepSession_RemovesBothHalvesOfASessionsDisk(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ctx := context.Background()
	origin := newTestOriginRepo(t)

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId: "task-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if err := os.MkdirAll(s.git.ClaudeHomePath("task-1"), 0o755); err != nil {
		t.Fatalf("mkdir claude home: %v", err)
	}

	if _, err := s.SweepSession(ctx, &agentfleetv1.SweepSessionRequest{SessionId: "task-1"}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}

	if _, err := k8sc.Core.CoreV1().PersistentVolumeClaims("agent-fleet").Get(ctx, k8s.SessionPVCName("task-1"), metav1.GetOptions{}); err == nil {
		t.Error("the working volume survived the sweep — the disk it was meant to reclaim is still allocated")
	}
	if _, err := os.Stat(s.git.ClaudeHomePath("task-1")); err == nil {
		t.Error("the SDK state survived the sweep — this is the half that leaks quietly, " +
			"since nothing else ever looks at it again")
	}

	// Sweeping again is a correct no-op: the GC retries a failed sweep on its
	// next pass, and by then one half may already be gone.
	if _, err := s.SweepSession(ctx, &agentfleetv1.SweepSessionRequest{SessionId: "task-1"}); err != nil {
		t.Fatalf("second SweepSession should be a no-op, got %v", err)
	}
}
