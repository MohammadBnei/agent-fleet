package grpcserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0063: the genome is checked before anything happens, written only
// with no live Job, and confirmed in the answer.
func TestCreateWorkerPod_Genome(t *testing.T) {
	s, k8sc, reporter := newTestServer(t)
	ctx := context.Background()
	origin := newTestOriginRepo(t)
	req := func(id string, g ...*agentfleetv1.GenomeFile) *agentfleetv1.CreateWorkerPodRequest {
		return &agentfleetv1.CreateWorkerPodRequest{SessionId: id, Repo: "dream-analyst", RepoUrl: origin, BaseBranch: "main", Genome: g}
	}

	for _, r := range []*agentfleetv1.CreateWorkerPodRequest{
		req("task-1", &agentfleetv1.GenomeFile{Path: ".claude/settings.json", Body: "{}"}),
		req("../task-1"),
		req("task-1-genome"),
	} {
		if _, err := s.CreateWorkerPod(ctx, r); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CreateWorkerPod(%s, %v) = %v, want InvalidArgument", r.GetSessionId(), r.GetGenome(), err)
		}
	}
	if len(reporter.events) != 0 {
		t.Fatalf("a refused request reported events: %+v", reporter.events)
	}

	resp, err := s.CreateWorkerPod(ctx, req("task-1", &agentfleetv1.GenomeFile{Path: ".claude/rules/20-style.md", Body: "short"}))
	if err != nil || !resp.GetGenomeApplied() {
		t.Fatalf("CreateWorkerPod = %v, %v; want genome_applied", resp, err)
	}
	if b, err := os.ReadFile(filepath.Join(s.git.GenomePath("task-1"), ".claude/rules/20-style.md")); err != nil || string(b) != "short" {
		t.Fatalf("genome file = %q, %v", b, err)
	}

	// The Job from that create is live: a second warm must not swap the
	// genome out from under it.
	if _, exists, _ := k8sc.GetWorkerJob(ctx, "task-1"); !exists {
		t.Fatal("expected the worker job to exist")
	}
	if _, err := s.CreateWorkerPod(ctx, req("task-1")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second CreateWorkerPod with a live Job = %v, want FailedPrecondition", err)
	}
	if _, err := os.Stat(filepath.Join(s.git.GenomePath("task-1"), ".claude/rules/20-style.md")); err != nil {
		t.Fatalf("the live pod's genome was replaced: %v", err)
	}
}
