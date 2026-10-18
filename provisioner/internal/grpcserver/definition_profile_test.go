package grpcserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestGetVersion_SaysItAppliesTheDefinitionProfile(t *testing.T) {
	s, _, _ := newTestServer(t)
	resp, err := s.GetVersion(context.Background(), &agentfleetv1.GetVersionRequest{})
	if err != nil || !resp.GetDefinitionPodProfile() {
		t.Fatalf("definition_pod_profile = %v, %v", resp.GetDefinitionPodProfile(), err)
	}
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
	s := New(k8sc, git.NewManager(root), &fakeEventReporter{}, "e2e.bnei.dev", fleetShared, "main", "test")
	origin := newTestOriginRepo(t)

	// A repo session first: it clones the cache and gets fleet-shared.
	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{SessionId: "repo-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-home", "repo-1", "settings.json")); err != nil {
		t.Fatalf("fixture: the repo session must get fleet-shared: %v", err)
	}
	commitTo(t, origin, "new.txt")

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId: "def-1", Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main",
		Definition: &agentfleetv1.DefinitionPodProfile{Name: "dev-dream-analyst", GitAuthorName: "a", GitAuthorEmail: "a@bnei.dev"},
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := revParse(t, filepath.Join(root, "repos", "dream-analyst"), "origin/main"), revParse(t, origin, "HEAD"); got != want {
		t.Errorf("cache origin/main %s, want the remote's tip %s: the definition clone reads it", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-home", "def-1")); err != nil {
		t.Errorf("claude-home still set up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-home", "def-1", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("a definition pod got fleet-shared settings.json: %v", err)
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
	ok := func() *agentfleetv1.DefinitionPodProfile {
		return &agentfleetv1.DefinitionPodProfile{Name: "d", GitAuthorName: "a", GitAuthorEmail: "a@bnei.dev"}
	}
	for name, mut := range map[string]func(*agentfleetv1.CreateWorkerPodRequest){
		"no name":   func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.Name = "" },
		"no author": func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.GitAuthorName = "" },
		"no email":  func(r *agentfleetv1.CreateWorkerPodRequest) { r.Definition.GitAuthorEmail = "" },
		"tool keys": func(r *agentfleetv1.CreateWorkerPodRequest) { r.ToolKeys = []string{"cluster-access"} },
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
