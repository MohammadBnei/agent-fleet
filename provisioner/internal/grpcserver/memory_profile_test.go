package grpcserver

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// docs/adr/0086 §5: a standing definition's pod gets its memory wiring from the stored set; others get none; a
// standing pod with no valid set is refused, so the pod never runs with memory it may not have.
func TestCreateWorkerPod_MemoryFollowsStandingAndSet(t *testing.T) {
	ctx := context.Background()
	s, k8sc, _ := newTestServer(t)
	origin := newTestOriginRepo(t)
	create := func(id string, standing bool, set string) error {
		p := profile("kai")
		p.Standing, p.TrustSet = standing, set
		_, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{SessionId: id, Repo: "r", RepoUrl: origin, BaseBranch: "main", Definition: p})
		return err
	}
	sidecarEnv := func(id string) string {
		job, err := k8sc.Core.BatchV1().Jobs("agent-fleet").Get(ctx, k8s.WorkerResourceName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var env []string
		for _, c := range job.Spec.Template.Spec.InitContainers {
			for _, e := range c.Env {
				env = append(env, e.Name+"="+e.Value)
			}
		}
		return strings.Join(env, " ")
	}
	if err := create("m1", true, "AB"); err != nil {
		t.Fatal(err)
	}
	if got := sidecarEnv("m1"); !strings.Contains(got, "FLEET_AGENT_NAME=kai") || !strings.Contains(got, "FLEET_TRUST_SET=AB") {
		t.Errorf("standing pod: no memory env: %s", got)
	}
	if err := create("m2", false, "AB"); err != nil {
		t.Fatal(err)
	}
	if got := sidecarEnv("m2"); strings.Contains(got, "FLEET_TRUST_SET") {
		t.Errorf("a non-standing pod got memory: %s", got)
	}
	if err := create("m3", true, ""); err == nil {
		t.Error("a standing pod with no trust set was created")
	}
}
