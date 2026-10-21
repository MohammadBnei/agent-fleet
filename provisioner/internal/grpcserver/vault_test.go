package grpcserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	s.vault = vault.New(f.URL, "agent-vault.agent-vault.svc.cluster.local:14322", dir)
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
// says so; the pod's deadline is the fixed 12h cap whatever limits.timeout
// says, and the token outlives it by 60s.
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
	if sess.TTL != 43260 || !strings.HasPrefix(sess.Label, "def-1.") {
		t.Errorf("session %+v, want ttl 43260 and label def-1.<ms>", sess)
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
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 43200 {
		t.Errorf("deadline %v, want the 12h cap, not limits.timeout (1800)", d)
	}
	if cm, err := k8sc.Core.CoreV1().ConfigMaps("agent-fleet").Get(context.Background(), k8s.VaultCAConfigMap, metav1.GetOptions{}); err != nil || cm.Data["ca.pem"] != vaulttest.CA {
		t.Errorf("CA configmap: %v", err)
	}

	// limits.timeout no longer reaches the pod: a 60s one gets the same cap.
	p := profile("dev-short")
	p.TimeoutSeconds = 60
	if _, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-2", p)); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("dev-short"); v.Sessions[0].TTL != 43260 {
		t.Errorf("ttl %d, want 12h+60", v.Sessions[0].TTL)
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
// BEFORE the Job delete, a repo session's on its owner vault (docs/adr/0075).
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
	f.AddSession("owner-mohammadbnei", "repo-2.1")
	if _, err := s.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{SessionId: "repo-1"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("owner-mohammadbnei"); len(v.Sessions) != 1 || v.Sessions[0].Label != "repo-2.1" {
		t.Errorf("owner vault sessions %+v, want only the other pod's", v.Sessions)
	}
}

// failJob marks a session's Job the way a crashed worker leaves it: Failed,
// and kept by reconcile for its TTL.
func failJob(t *testing.T, k8sc *k8s.Client, id string) {
	t.Helper()
	jobs := k8sc.Core.BatchV1().Jobs("agent-fleet")
	job, err := jobs.Get(context.Background(), k8s.WorkerResourceName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if _, err := jobs.UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A crashed session's Job is kept for its TTL under the session's own Job
// name, so a re-warm used to die on AlreadyExists. Now a terminal Job is
// replaced: its token revoked before its delete, then the new pod created. A
// live Job is still refused.
func TestCreateWorkerPod_RewarmReplacesTerminalJob(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	ctx := context.Background()
	req := defRequest(t, "def-1", profile("dev-x"))
	if _, err := s.CreateWorkerPod(ctx, req); err != nil {
		t.Fatal(err)
	}
	old, _, _ := k8sc.GetWorkerJob(ctx, "def-1")

	// Live: refused, the running pod and its token untouched.
	if _, err := s.CreateWorkerPod(ctx, req); err == nil {
		t.Fatal("a re-warm over a live Job succeeded")
	}
	if v, _ := f.Get("dev-x"); len(v.Sessions) != 1 || v.Sessions[0].Label != old.VaultLabel {
		t.Fatalf("live pod's token not kept alone: %+v", v.Sessions)
	}

	failJob(t, k8sc, "def-1")
	oldLiveAtDelete := true
	k8sc.Core.(*fake.Clientset).PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		v, _ := f.Get("dev-x")
		oldLiveAtDelete = slices.ContainsFunc(v.Sessions, func(s vaulttest.Session) bool { return s.Label == old.VaultLabel })
		return false, nil, nil
	})
	if _, err := s.CreateWorkerPod(ctx, req); err != nil {
		t.Fatalf("re-warm after a crash: %v", err)
	}
	if oldLiveAtDelete {
		t.Error("the kept Job's token was still live at its delete")
	}
	if j, exists, _ := k8sc.GetWorkerJob(ctx, "def-1"); !exists || j.Terminal() || j.VaultLabel == old.VaultLabel {
		t.Errorf("new Job %+v exists=%v, want a fresh live one", j, exists)
	}

	// A repo session's kept Job is replaced the same way, on its owner vault.
	repo := &agentfleetv1.CreateWorkerPodRequest{SessionId: "repo-1", Repo: "repo-1", RepoUrl: newTestOriginRepo(t), BaseBranch: "main"}
	if _, err := s.CreateWorkerPod(ctx, repo); err != nil {
		t.Fatal(err)
	}
	oldRepo, _, _ := k8sc.GetWorkerJob(ctx, "repo-1")
	failJob(t, k8sc, "repo-1")
	if _, err := s.CreateWorkerPod(ctx, repo); err != nil {
		t.Fatalf("repo re-warm after a crash: %v", err)
	}
	v, _ := f.Get("owner-mohammadbnei")
	if len(v.Sessions) != 1 || v.Sessions[0].Label == oldRepo.VaultLabel || oldRepo.VaultOwner != "owner-mohammadbnei" {
		t.Errorf("owner vault sessions %+v, want only the re-warm's (old %q)", v.Sessions, oldRepo.VaultLabel)
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

// docs/adr/0065 §3: a widening revokes every token on the vault, so the
// definition's live pods are ended (core re-warms them at the new spec); another
// definition's pod is untouched.
func TestReconcileVaults_WideningEndsLivePods(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth", "GH_DEV_NOTES": "pat"})
	ctx := context.Background()
	for id, p := range map[string]*agentfleetv1.DefinitionPodProfile{"def-1": profile("dev-notes"), "def-2": profile("dev-x")} {
		if _, err := s.CreateWorkerPod(ctx, defRequest(t, id, p)); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{notesProfile().Vault, profile("dev-x").Vault}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.GetResults() {
		if r.GetError() != "" {
			t.Errorf("%s: %s", r.GetName(), r.GetError())
		}
	}
	for id, want := range map[string]bool{"def-1": false, "def-2": true} {
		if _, exists, err := k8sc.GetWorkerJob(ctx, id); err != nil || exists != want {
			t.Errorf("%s: exists=%v err=%v, want exists=%v", id, exists, err, want)
		}
	}
}

// lockHeld reports whether someone holds vault name's lock: a Lock attempt
// that does not get it within 100ms. The attempt then waits it out.
func lockHeld(s *Server, name string) bool {
	got := make(chan struct{})
	go func() {
		s.vault.Lock(name)()
		close(got)
	}()
	select {
	case <-got:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

// docs/adr/0065 §3: a warm holds the vault's lock through its Job create and a
// widening holds it through ending the definition's Jobs, so a widening can
// neither land between a warm's reconcile and its mint (a pod on an old pin
// with a wider token) nor list Jobs before a revoked warm's pod exists.
func TestVaultLock_SpansWarmAndWidening(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth", "GH_DEV_NOTES": "pat"})
	ctx := context.Background()
	var atCreate, atDelete bool
	k8sc.Core.(*fake.Clientset).PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		atCreate = lockHeld(s, "dev-notes")
		return false, nil, nil
	})
	k8sc.Core.(*fake.Clientset).PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		atDelete = lockHeld(s, "dev-notes")
		return false, nil, nil
	})
	if _, err := s.CreateWorkerPod(ctx, defRequest(t, "def-1", profile("dev-notes"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{notesProfile().Vault}}); err != nil {
		t.Fatal(err)
	}
	if !atCreate || !atDelete {
		t.Errorf("vault lock held at Job create=%v, at widening's Job delete=%v; want both", atCreate, atDelete)
	}
}

// docs/adr/0065 §5: no egress lock, no token and no pod.
func TestCreateWorkerPod_DefinitionRefusedWithoutEgressLock(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	if err := k8sc.Dynamic.Resource(k8s.CNPGVR).Namespace("agent-fleet").Delete(context.Background(), k8s.EgressLockName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateWorkerPod(context.Background(), defRequest(t, "def-1", profile("dev-x")))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), k8s.EgressLockName) {
		t.Fatalf("got %v", err)
	}
	if n := jobCount(t, k8sc); n != 0 {
		t.Errorf("%d jobs", n)
	}
	if v, _ := f.Get("dev-x"); len(v.Sessions) != 0 {
		t.Error("a token was minted without an egress lock")
	}
}

type rotatingForge struct{ token string }

func (f *rotatingForge) Token(context.Context, string, string) (string, error) { return f.token, nil }

// docs/adr/0075 §5: the provisioner pushes re-minted tokens on its own, so a
// stalled core never lets a vault's GitHub token expire.
func TestRefreshVaults_PushesTheReMintedTokenWithoutCore(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	forge := &rotatingForge{token: "ghs-1"}
	s.vault.Forge = forge
	ctx := context.Background()
	if _, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{notesProfile().Vault}}); err != nil {
		t.Fatal(err)
	}
	forge.token = "ghs-2"
	s.RefreshVaults(ctx)
	if v, _ := f.Get("dev-notes"); v.Creds["GH_DEV_NOTES"] != "ghs-2" {
		t.Errorf("GH_DEV_NOTES = %q after refresh, want ghs-2", v.Creds["GH_DEV_NOTES"])
	}
}

func TestRefreshVaults_NothingBeforeCoreSentASet(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{})
	s.RefreshVaults(context.Background())
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("refreshed with no known specs: %v", calls)
	}
}

// A refresh that waited on the vault lock must use core's newest set: a vault
// core dropped meanwhile is not brought back.
func TestRefreshVault_UsesTheNewestSpecNotItsSnapshot(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := withVault(t, s, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"})
	s.vault.Forge = &rotatingForge{token: "ghs-1"}
	ctx := context.Background()
	if _, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{Vaults: []*agentfleetv1.VaultSpec{notesProfile().Vault}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileVaults(ctx, &agentfleetv1.ReconcileVaultsRequest{}); err != nil { // core drops dev-notes, Prune empties it
		t.Fatal(err)
	}
	f.ResetLog()
	if err := s.refreshVault(ctx, "dev-notes"); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("a stale refresh rewrote a pruned vault: %v", w)
	}
}

func repoRequest(t *testing.T, id string) *agentfleetv1.CreateWorkerPodRequest {
	return &agentfleetv1.CreateWorkerPodRequest{SessionId: id, Repo: id, RepoUrl: newTestOriginRepo(t), BaseBranch: "main"}
}

func workerEnv(t *testing.T, k8sc *k8s.Client, id string) map[string]string {
	t.Helper()
	job, err := k8sc.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), k8s.WorkerResourceName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"label:" + k8s.VaultOwnerLabel: job.Spec.Template.Labels[k8s.VaultOwnerLabel]}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if d := job.Spec.ActiveDeadlineSeconds; d != nil {
		env["deadline"] = strconv.FormatInt(*d, 10)
	}
	return env
}

// docs/adr/0075 §7: a repo warm reconciles its owner's vault (passthrough,
// the owner token) and mints the pod's token on it; the pod holds only
// placeholders, the commit identity from config and the 12h cap.
func TestCreateWorkerPod_RepoSessionUsesItsOwnerVault(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp-bot-sentinel")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-env-sentinel")
	s, k8sc, _ := newTestServer(t)
	f := withRepoVault(t, s)
	resp, err := s.CreateWorkerPod(context.Background(), repoRequest(t, "repo-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetVaultTokenApplied() || resp.GetDefinitionProfileApplied() {
		t.Errorf("response %v", resp)
	}
	v, ok := f.Get("owner-mohammadbnei")
	if !ok || v.Policy != "passthrough" || v.Creds["GH_OWNER_MOHAMMADBNEI"] != "ghs-owner" || len(v.Sessions) != 1 || v.Sessions[0].TTL != 43260 {
		t.Fatalf("owner vault %+v", v)
	}
	env := workerEnv(t, k8sc, "repo-1")
	// The worker proxies through the sidecar's egress router; only the
	// sidecar holds the token.
	if env["HTTPS_PROXY"] != "http://127.0.0.1:9093" {
		t.Errorf("HTTPS_PROXY %q", env["HTTPS_PROXY"])
	}
	for k, val := range env {
		if strings.Contains(val, v.Sessions[0].Token) {
			t.Errorf("worker env %s holds the vault token", k)
		}
	}
	job, _ := k8sc.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), k8s.WorkerResourceName("repo-1"), metav1.GetOptions{})
	sidecar := map[string]string{}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name == "sidecar" {
			for _, e := range c.Env {
				sidecar[e.Name] = e.Value
			}
		}
	}
	if sidecar["FLEET_VAULT_PROXY"] != "http://"+v.Sessions[0].Token+":owner-mohammadbnei@agent-vault.agent-vault.svc.cluster.local:14322" ||
		sidecar["FLEET_VAULT_HOSTS"] != "api.anthropic.com,github.com,api.github.com" || sidecar["EGRESS_PORT"] != "9093" {
		t.Errorf("sidecar env %v", sidecar)
	}
	for k, want := range map[string]string{
		"GH_TOKEN": k8s.VaultPlaceholder, "CLAUDE_CODE_OAUTH_TOKEN": k8s.VaultPlaceholder,
		"GIT_AUTHOR_EMAIL": "ukubi@bnei.dev", "SSL_CERT_FILE": "/etc/agent-vault/bundle.pem",
		"deadline": "43200", "label:" + k8s.VaultOwnerLabel: "owner-mohammadbnei",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["LEASE_ID"]; ok {
		t.Error("LEASE_ID reached the worker container")
	}
	if _, ok := env["FLEET_DEFINITION"]; ok {
		t.Error("a repo pod got FLEET_DEFINITION")
	}
	cm, err := k8sc.Core.CoreV1().ConfigMaps("agent-fleet").Get(context.Background(), k8s.VaultCAConfigMap, metav1.GetOptions{})
	if err != nil || !strings.HasSuffix(cm.Data["bundle.pem"], vaulttest.CA) || len(cm.Data["bundle.pem"]) <= len(vaulttest.CA)+1 {
		t.Errorf("CA bundle: %v", err)
	}
}

// No fallback (docs/adr/0075 §10): an owner token that cannot be minted
// refuses the warm before any vault write or Job.
func TestCreateWorkerPod_RepoOwnerMintFailureRefusesTheWarm(t *testing.T) {
	s, k8sc, _ := newTestServer(t)
	f := withRepoVault(t, s)
	s.SetGitHub(&fakeApp{err: errors.New("github down")})
	_, err := s.CreateWorkerPod(context.Background(), repoRequest(t, "repo-1"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err %v", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("vault writes %v", w)
	}
	if n := jobCount(t, k8sc); n != 0 {
		t.Errorf("%d Jobs", n)
	}
}

// RefreshVaults pushes a re-minted owner token with no core call at all.
func TestRefreshVaults_PushesOwnerTokens(t *testing.T) {
	s, _, _ := newTestServer(t)
	f := withRepoVault(t, s)
	app := &fakeApp{token: "ghs-1"}
	s.SetGitHub(app)
	s.RefreshVaults(context.Background())
	app.token = "ghs-2"
	s.RefreshVaults(context.Background())
	if v, _ := f.Get("owner-mohammadbnei"); v.Creds["GH_OWNER_MOHAMMADBNEI"] != "ghs-2" {
		t.Errorf("owner vault creds %v", v.Creds)
	}
}

func TestRepoOwner(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/MohammadBnei/agent-fleet.git": "MohammadBnei",
		"https://github.com/opus-tech-hq/workspace":       "opus-tech-hq",
		"https://github.com/o/r/":                         "o",
		"/tmp/x/MohammadBnei/origin":                      "MohammadBnei",
		"repo":                                            "",
		"https://github.com/a_b/r.git":                    "",
		"https://evil.example/MohammadBnei/r":             "",
		"http://github.com/MohammadBnei/r":                "",
	} {
		got, err := repoOwner(url)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("repoOwner(%q) = %q, %v; want %q", url, got, err, want)
		}
	}
}
