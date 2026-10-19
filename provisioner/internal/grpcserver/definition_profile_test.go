package grpcserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

// docs/adr/0064: a definition pod gets the profile, a fetched cache (its clone
// never contacts the remote) and no fleet-shared settings.json (which
// pre-approves arbitrary-exec Bash past canUseTool).
func TestCreateWorkerPod_DefinitionProfile(t *testing.T) {
	ctx := context.Background()
	k8sc := newFakeK8sClient()
	root := t.TempDir()
	fleetShared := t.TempDir()
	gitIn(t, fleetShared, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(fleetShared, "fleet-shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitTo(t, fleetShared, "fleet-shared/settings.json")
	s := New(k8sc, git.NewManager(root), &fakeEventReporter{}, nil, "e2e.bnei.dev", fleetShared, "main", "test")
	withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	origin := newTestOriginRepo(t)

	// A repo session first: it clones the cache and gets fleet-shared.
	if resp, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{SessionId: "repo-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main"}); err != nil || resp.GetDefinitionProfileApplied() {
		t.Fatalf("repo session: applied=%v err=%v", resp.GetDefinitionProfileApplied(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-home", "repo-1", "settings.json")); err != nil {
		t.Fatalf("fixture: the repo session must get fleet-shared: %v", err)
	}
	commitTo(t, origin, "new.txt")
	// What an older replica's unconditional sync left in def-1's claude-home
	// (a rollout overlap): it must be gone before the pod exists. projects/
	// is resume state and stays.
	defHome := filepath.Join(root, "claude-home", "def-1")
	for _, p := range []string{"settings.json", "CLAUDE.md", "skills/x/SKILL.md", "projects/p/s.jsonl"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(defHome, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(defHome, p), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A busy cache (a concurrent sync holds its lock): a repo session's create
	// would skip the fetch and return at once; a definition's must wait and
	// fetch, since its clone never contacts the remote.
	unlock := s.git.LockRepo("dream-analyst")
	type result struct {
		resp *agentfleetv1.CreateWorkerPodResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
			SessionId: "def-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
			Definition: profile("dev-dream-analyst"),
		})
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		unlock()
		t.Fatalf("the definition create skipped the busy cache's fetch: %v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	r := <-done
	resp, err := r.resp, r.err
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetDefinitionProfileApplied() {
		t.Error("the response must confirm the profile: core tears down a definition warm without it")
	}
	if got, want := revParse(t, filepath.Join(root, "repos", "dream-analyst"), "origin/main"), revParse(t, origin, "HEAD"); got != want {
		t.Errorf("cache origin/main %s, want the remote's tip %s: the definition clone reads it", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-home", "def-1")); err != nil {
		t.Errorf("claude-home still set up: %v", err)
	}
	for _, name := range []string{"settings.json", "CLAUDE.md", "skills"} {
		if _, err := os.Stat(filepath.Join(defHome, name)); !os.IsNotExist(err) {
			t.Errorf("a definition pod got fleet-shared %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(defHome, "projects", "p", "s.jsonl")); err != nil {
		t.Errorf("resume state removed: %v", err)
	}
	job, err := k8sc.Core.BatchV1().Jobs("agent-fleet").Get(ctx, k8s.WorkerResourceName("def-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Labels[k8s.DefinitionLabel] != "dev-dream-analyst" {
		t.Errorf("profile not applied: labels %v", job.Labels)
	}
}

func TestCreateWorkerPod_RejectsAnIncompleteProfile(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	ok := func() *agentfleetv1.DefinitionPodProfile { return profile("ddd") }
	for name, mut := range map[string]func(*agentfleetv1.CreateWorkerPodRequest){
		"no vault":      func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.Vault = nil },
		"another vault": func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.Vault.Name = "dev-notes" },
		"no timeout":    func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.TimeoutSeconds = 0 },
		"no name":       func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.Name = "" },
		"no author":     func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.GitAuthorName = "" },
		"no email":      func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.GitAuthorEmail = "" },
		"tool keys":     func(r *agentfleetv1.CreateWorkerPodRequest) { r.ToolKeys = []string{"cluster-access"} },
		"image":         func(r *agentfleetv1.CreateWorkerPodRequest) { r.Image = "registry.bnei.lan:5000/old-worker:1" },
	} {
		req := &agentfleetv1.CreateWorkerPodRequest{SessionId: "s", Repo: "r", RepoUrl: "/nonexistent", Definition: ok()}
		mut(req)
		if _, err := s.CreateWorkerPod(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", name, err)
		}
	}
	if jobs, _ := k8sc.Core.BatchV1().Jobs("agent-fleet").List(context.Background(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatalf("a refused profile created %d jobs", len(jobs.Items))
	}
}
