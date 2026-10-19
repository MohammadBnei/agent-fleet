package grpcserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault/vaulttest"
)

// withVault points s at a fake Agent Vault holding the agent token and secrets.
func withVault(t *testing.T, s *Server, secrets map[string]string) *vaulttest.Server {
	t.Helper()
	f := vaulttest.New(t)
	dir := t.TempDir()
	secrets["AGENT_VAULT_TOKEN"] = vaulttest.Token
	for k, v := range secrets {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.SetVault(vault.New(f.URL, "agent-vault.agent-vault.svc.cluster.local:14322", dir))
	return f
}

func profile(name string) *agentfleetv1.DefinitionPodProfile {
	return &agentfleetv1.DefinitionPodProfile{
		Name: name, GitAuthorName: "a", GitAuthorEmail: "a@bnei.dev", TimeoutSeconds: 1800,
		Vault: &agentfleetv1.VaultSpec{Name: name, Services: []*agentfleetv1.VaultService{{Host: "api.anthropic.com", Secret: "CLAUDE_CODE_OAUTH_TOKEN"}}},
	}
}

func notesProfile() *agentfleetv1.DefinitionPodProfile {
	p := profile("dev-notes")
	p.Vault.Services = append(p.Vault.Services, &agentfleetv1.VaultService{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*"}, Secret: "GH_DEV_NOTES"})
	return p
}

func defRequest(t *testing.T, id string, p *agentfleetv1.DefinitionPodProfile) *agentfleetv1.CreateWorkerPodRequest {
	return &agentfleetv1.CreateWorkerPodRequest{SessionId: id, Repo: id, RepoUrl: newTestOriginRepo(t), BaseBranch: "main", Definition: p}
}

func jobCount(t *testing.T, k8sc *k8s.Client) int {
	t.Helper()
	jobs, err := k8sc.Core.BatchV1().Jobs("agent-fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs.Items)
}

// docs/adr/0065: a warm mints one token, after the vault matches the spec, and
// says so; the token outlives the pod's deadline by 60s.
func TestCreateWorkerPod_DefinitionMintsVaultToken(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth", "GH_DEV_NOTES": "pat"})
	resp, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-1", notesProfile()))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetVaultTokenApplied() || !resp.GetDefinitionProfileApplied() {
		t.Fatalf("response %v", resp)
	}
	v, ok := f.Get("dev-notes")
	if !ok || v.Policy != "deny" || len(v.Sessions) != 1 {
		t.Fatalf("vault %+v", v)
	}
	sess := v.Sessions[0]
	if sess.TTL != 1860 || !strings.HasPrefix(sess.Label, "def-1.") {
		t.Errorf("session %+v, want ttl 1860 and label def-1.<ms>", sess)
	}
	job, err := k8sc.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), k8s.WorkerResourceName("def-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var proxy string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "HTTPS_PROXY" {
			proxy = e.Value
		}
	}
	if proxy != "http://"+sess.Token+":dev-notes@agent-vault.agent-vault.svc.cluster.local:14322" {
		t.Errorf("HTTPS_PROXY %q does not carry the minted token", proxy)
	}
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 1800 {
		t.Errorf("deadline %v", d)
	}
	if cm, err := k8sc.Core.CoreV1().ConfigMaps("agent-fleet").Get(context.Background(), k8s.VaultCAConfigMap, metav1.GetOptions{}); err != nil || cm.Data["ca.pem"] != vaulttest.CA {
		t.Errorf("CA configmap: %v", err)
	}

	// The deadline floor: a 60s timeout still gets the vault's 300s minimum.
	p := profile("dev-short")
	p.TimeoutSeconds = 60
	if _, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-2", p)); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("dev-short"); v.Sessions[0].TTL != 360 {
		t.Errorf("ttl %d, want 300+60", v.Sessions[0].TTL)
	}
}

// A missing secret refuses the warm with the key's name, before any pod or
// token exists.
func TestCreateWorkerPod_DefinitionMissingSecretRefused(t *testing.T) {
	s, k8sc, reporter := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	_, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-1", notesProfile()))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "GH_DEV_NOTES") {
		t.Fatalf("got %v", err)
	}
	if n := jobCount(t, k8sc); n != 0 {
		t.Errorf("%d jobs", n)
	}
	if v, _ := f.Get("dev-notes"); len(v.Sessions) != 0 {
		t.Error("a token was minted for a refused warm")
	}
	last := reporter.events[len(reporter.events)-1]
	if last.GetPhase() != agentfleetv1.PodPhase_POD_PHASE_CRASHED || !strings.Contains(last.GetMessage(), "GH_DEV_NOTES") {
		t.Errorf("last event %v", last)
	}
}

func TestCreateWorkerPod_DefinitionRevokesWhenJobCreateFails(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	k8sc.Core.(*fake.Clientset).PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission said no")
	})
	if _, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-1", profile("dev-x"))); err == nil {
		t.Fatal("create succeeded")
	}
	if v, _ := f.Get("dev-x"); len(v.Sessions) != 0 {
		t.Errorf("the minted token survived a failed Job create: %+v", v.Sessions)
	}
}

// Teardown revokes exactly the pod's own token (its label is on the Job)
// BEFORE the Job delete; a repo session's teardown never calls the vault.
func TestTearDownSession_RevokesBeforeDelete(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	ctx := context.Background()
	if _, err := s.CreateWorkerPod(ctx, defRequest(t, "def-1", profile("dev-x"))); err != nil {
		t.Fatal(err)
	}
	f.AddSession("dev-x", "other.1")
	f.AddSession("dev-x", "def-1.1") // same session, another warm's label: not this pod's
	liveAtDelete := -1
	k8sc.Core.(*fake.Clientset).PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		v, _ := f.Get("dev-x")
		liveAtDelete = len(v.Sessions)
		return false, nil, nil
	})
	if _, err := s.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{SessionId: "def-1"}); err != nil {
		t.Fatal(err)
	}
	if liveAtDelete != 2 {
		t.Errorf("%d sessions live at the Job delete, want the two unminted-here ones", liveAtDelete)
	}

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{SessionId: "repo-1", Repo: "repo-1", RepoUrl: newTestOriginRepo(t), BaseBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	f.ResetLog()
	if _, err := s.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{SessionId: "repo-1"}); err != nil {
		t.Fatal(err)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("a repo session's teardown called the vault: %v", calls)
	}
}

func TestReconcileVaults_ResultsAndPrune(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	ctx := context.Background()
	if _, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{profile("old-one").Vault}}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{profile("dev-x").Vault, notesProfile().Vault}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range resp.GetResults() {
		got[r.GetName()] = r.GetError()
	}
	if got["dev-x"] != "" || !strings.Contains(got["dev-notes"], "GH_DEV_NOTES") || len(got) != 2 {
		t.Errorf("results %v", got)
	}
	if resp.GetPruneError() != "" {
		t.Errorf("prune error %q", resp.GetPruneError())
	}
	if v, _ := f.Get("old-one"); len(v.Creds) != 0 || string(v.Services) != "[]" {
		t.Errorf("a vault absent from the full set was not emptied: %+v", v)
	}
	if !slices.Contains(f.Names(), "dev-x") {
		t.Error("dev-x not created")
	}
}
