package k8s

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func definitionSpec() WorkerPodSpec {
	return WorkerPodSpec{
		SessionID: "task-1", Repo: "dream-analyst", RepoURL: "https://github.com/o/dream-analyst.git", BaseBranch: "main",
		LeaseID: "lease-1",
		Definition: &DefinitionProfile{Name: "dev-dream-analyst", GitAuthorName: "fleet-dev-agent", GitAuthorEmail: "fleet-dev-agent@bnei.dev",
			ProxyURL: testProxyURL, DeadlineSeconds: 1800},
	}
}

const testProxyURL = "http://av_sess_x:dev-dream-analyst@agent-vault.agent-vault.svc.cluster.local:14322"

func createJob(t *testing.T, spec WorkerPodSpec) *batchv1.Job {
	t.Helper()
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	c := newTestClient()
	c.CoreGRPCAddr = "agent-fleet-core.agent-fleet.svc.cluster.local:9090"
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, spec); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(ctx, WorkerResourceName(spec.SessionID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func container(t *testing.T, job *batchv1.Job, name string) corev1.Container {
	t.Helper()
	ps := job.Spec.Template.Spec
	for _, c := range append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

// docs/adr/0064: what a definition pod's worker must not hold, and what it
// must. docs/adr/0065 flipped the model credential to a placeholder: the vault
// injects the real one on the wire.
func TestCreateWorkerPod_DefinitionProfile(t *testing.T) {
	job := createJob(t, definitionSpec())

	worker := container(t, job, "worker")
	if _, ok := findEnv(worker.Env, "LEASE_ID"); ok {
		t.Error("definition worker has LEASE_ID")
	}
	for name, want := range map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": VaultPlaceholder, // docs/adr/0065: the vault overwrites Authorization
		"GH_TOKEN":                VaultPlaceholder,
		"FLEET_DEFINITION":        "dev-dream-analyst", // the worker closes the native cross-session channel
		"GIT_AUTHOR_NAME":         "fleet-dev-agent",
		"GIT_COMMITTER_NAME":      "fleet-dev-agent",
		"GIT_AUTHOR_EMAIL":        "fleet-dev-agent@bnei.dev",
		"GIT_COMMITTER_EMAIL":     "fleet-dev-agent@bnei.dev",
		"GIT_CONFIG_COUNT":        "1",
		"GIT_CONFIG_KEY_0":        "safe.directory",
		"GIT_CONFIG_VALUE_0":      "/workspace",
	} {
		if got, _ := findEnv(worker.Env, name); got != want {
			t.Errorf("worker %s = %q, want %q", name, got, want)
		}
	}
	if got, _ := findEnv(container(t, job, "sidecar").Env, "LEASE_ID"); got != "lease-1" {
		t.Errorf("sidecar LEASE_ID = %q: it is the lease's only holder", got)
	}

	for _, name := range []string{"clone", "sidecar", "worker"} {
		var cache []corev1.VolumeMount
		for _, m := range container(t, job, name).VolumeMounts {
			if m.Name == "shared" && strings.HasPrefix(m.SubPath, "repos") {
				cache = append(cache, m)
			}
		}
		if len(cache) != 1 || cache[0].MountPath != "/repo-cache/dream-analyst" || cache[0].SubPath != "repos/dream-analyst" || !cache[0].ReadOnly {
			t.Errorf("%s repo-cache mounts = %+v, want only /repo-cache/dream-analyst <- repos/dream-analyst, read-only", name, cache)
		}
	}

	if job.Labels[DefinitionLabel] != "dev-dream-analyst" || job.Spec.Template.Labels[DefinitionLabel] != "dev-dream-analyst" {
		t.Errorf("definition label: job %v, pod %v", job.Labels, job.Spec.Template.Labels)
	}
}

// docs/adr/0065: after P3 no container of a definition pod, init containers
// included, holds a real secret. Sentinels stand in for every credential the
// provisioner's own environment carries. Deliberately excluded: the pod's two
// per-session credentials, its av_sess_ (the worker's HTTPS_PROXY) and the
// sidecar's LEASE_ID (CoreService, ADR-0057), both minted for this pod.
func TestDefinitionPod_NoRealSecret(t *testing.T) {
	sentinels := map[string]string{
		"GH_TOKEN": "sentinel-gh", "CLAUDE_CODE_OAUTH_TOKEN": "sentinel-oauth", "THOT_AUTH_TOKEN": "sentinel-thot",
		"FLEET_PROVISIONER_TOKEN": "sentinel-prov", "FLEET_CORE_PROVISIONER_TOKEN": "sentinel-core-prov",
		"AGENTFLEET_DB_PASSWORD": "sentinel-db", "AGENT_VAULT_TOKEN": "sentinel-av",
	}
	for k, v := range sentinels {
		t.Setenv(k, v)
	}
	c := newTestClient()
	c.ThotAuthToken = "sentinel-thot"
	if err := c.CreateWorkerPod(context.Background(), definitionSpec()); err != nil {
		t.Fatal(err)
	}
	job, err := c.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), WorkerResourceName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ps := job.Spec.Template.Spec
	for _, ctr := range append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...) {
		for _, e := range ctr.Env {
			for _, v := range sentinels {
				if strings.Contains(e.Value, v) {
					t.Errorf("%s %s carries a real secret", ctr.Name, e.Name)
				}
			}
			if e.ValueFrom != nil {
				t.Errorf("%s %s reads from a Secret/ConfigMap ref", ctr.Name, e.Name)
			}
		}
		if len(ctr.EnvFrom) > 0 {
			t.Errorf("%s has envFrom", ctr.Name)
		}
	}
	for _, v := range ps.Volumes {
		if v.Secret != nil {
			t.Errorf("volume %s mounts a Secret", v.Name)
		}
	}
	// The ServiceAccount token is admission-mounted, so the template is the
	// only place to see it refused.
	if a := ps.AutomountServiceAccountToken; a == nil || *a {
		t.Error("the pod would mount an API-server token")
	}
	if l := ps.EnableServiceLinks; l == nil || *l {
		t.Error("Service env vars would leak topology")
	}
}

// docs/adr/0065 §4: HTTPS_PROXY (both cases) on the worker only and no
// HTTP_PROXY (the vault injects on plain http too), NODE_USE_ENV_PROXY, the
// caller's NO_PROXY, the CA in five variables and mounted from the CA ConfigMap.
func TestDefinitionPod_ProxyAndCAEnv(t *testing.T) {
	job := createJob(t, definitionSpec())
	worker := container(t, job, "worker")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		if got, _ := findEnv(worker.Env, k); got != testProxyURL {
			t.Errorf("worker %s = %q", k, got)
		}
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy"} {
		if _, ok := findEnv(worker.Env, k); ok {
			t.Errorf("worker has %s: plain http would reach the vault and get a credential", k)
		}
	}
	if got, _ := findEnv(worker.Env, "NODE_USE_ENV_PROXY"); got != "1" {
		t.Errorf("worker NODE_USE_ENV_PROXY = %q", got)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		if got, _ := findEnv(worker.Env, k); got != "127.0.0.1,localhost,agent-fleet-core.agent-fleet.svc.cluster.local,agent-vault.agent-vault.svc.cluster.local" {
			t.Errorf("worker %s = %q", k, got)
		}
	}
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if got, _ := findEnv(worker.Env, k); got != "/etc/agent-vault/ca.pem" {
			t.Errorf("worker %s = %q", k, got)
		}
	}
	for k, want := range map[string]string{"GIT_TERMINAL_PROMPT": "0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"} {
		if got, _ := findEnv(worker.Env, k); got != want {
			t.Errorf("worker %s = %q", k, got)
		}
	}
	var mounted, skills bool
	for _, m := range worker.VolumeMounts {
		mounted = mounted || (m.MountPath == "/etc/agent-vault" && m.ReadOnly)
		// docs/adr/0067 §4: pack skills at user scope, read-only.
		if m.MountPath == "/claude-home/skills" {
			skills = m.ReadOnly && strings.HasSuffix(m.SubPath, "-genome/.claude/skills")
		}
	}
	if !skills {
		t.Errorf("worker has no read-only /claude-home/skills genome mount: %+v", worker.VolumeMounts)
	}
	var vol *corev1.Volume
	for i, v := range job.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == VaultCAConfigMap {
			vol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if !mounted || vol == nil || (vol.ConfigMap.Optional != nil && *vol.ConfigMap.Optional) {
		t.Errorf("CA not mounted read-only from a required %s ConfigMap", VaultCAConfigMap)
	}
	for _, name := range []string{"sidecar", "clone"} {
		for _, e := range container(t, job, name).Env {
			if strings.HasSuffix(strings.ToUpper(e.Name), "_PROXY") {
				t.Errorf("%s has %s: grpc-go honours it and the sidecar dials core directly", name, e.Name)
			}
		}
	}
}

func TestDefinitionPod_DNSAndDeadline(t *testing.T) {
	job := createJob(t, definitionSpec())
	ps := job.Spec.Template.Spec
	if ps.DNSConfig == nil || len(ps.DNSConfig.Options) != 1 || ps.DNSConfig.Options[0].Name != "ndots" || *ps.DNSConfig.Options[0].Value != "1" {
		t.Errorf("dnsConfig %+v, want ndots:1", ps.DNSConfig)
	}
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 1800 {
		t.Errorf("activeDeadlineSeconds %v, want 1800", d)
	}
	if job.Spec.Template.Labels[DefinitionLabel] != "dev-dream-analyst" {
		t.Error("no definition label: the egress lock selects on it")
	}
	// A repo pod gets neither.
	c := newTestClient()
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "r", Repo: "x"}); err != nil {
		t.Fatal(err)
	}
	repo, _ := c.Core.BatchV1().Jobs("agent-fleet").Get(context.Background(), WorkerResourceName("r"), metav1.GetOptions{})
	if repo.Spec.ActiveDeadlineSeconds != nil || repo.Spec.Template.Spec.DNSConfig != nil {
		t.Error("a repo pod got the definition's deadline or dnsConfig")
	}
	// No proxy URL = no vault token: never a definition pod.
	spec := definitionSpec()
	spec.Definition.ProxyURL = ""
	if err := c.CreateWorkerPod(context.Background(), spec); err == nil {
		t.Error("a definition pod without a vault token was built")
	}
}

func TestJobPhase_DeadlineExceeded(t *testing.T) {
	for reason, want := range map[string]string{"DeadlineExceeded": "DeadlineExceeded", "BackoffLimitExceeded": "Failed"} {
		job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True", Reason: reason}}}}
		if got := jobPhase(job); got != want {
			t.Errorf("%s: %q, want %q", reason, got, want)
		}
	}
}

func TestEnsureVaultCA_UpsertOnlyOnChange(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	for _, pem := range []string{"a", "a", "b"} {
		if err := c.EnsureVaultCA(ctx, []byte(pem)); err != nil {
			t.Fatal(err)
		}
	}
	cm, err := c.Core.CoreV1().ConfigMaps("agent-fleet").Get(ctx, VaultCAConfigMap, metav1.GetOptions{})
	if err != nil || cm.Data["ca.pem"] != "b" {
		t.Fatalf("%v %v", cm, err)
	}
	var writes int
	for _, a := range c.Core.(*fake.Clientset).Actions() {
		if a.GetResource().Resource == "configmaps" && (a.GetVerb() == "create" || a.GetVerb() == "update") {
			writes++
		}
	}
	if writes != 2 {
		t.Errorf("%d writes, want create + one update", writes)
	}
}

// Two first warms race: the loser's create gets AlreadyExists, a stale update
// gets Conflict. Both re-read and succeed instead of refusing the warm.
func TestEnsureVaultCA_ConcurrentWarms(t *testing.T) {
	c := newTestClient()
	cs := c.Core.(*fake.Clientset)
	ctx := context.Background()
	gr := schema.GroupResource{Resource: "configmaps"}
	created, conflicted := false, false
	cs.PrependReactor("create", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if created {
			return false, nil, nil
		}
		created = true
		_ = cs.Tracker().Add(a.(k8stesting.CreateAction).GetObject()) // the other warm won
		return true, nil, apierrors.NewAlreadyExists(gr, VaultCAConfigMap)
	})
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		return true, nil, apierrors.NewConflict(gr, VaultCAConfigMap, nil)
	})
	for _, pem := range []string{"a", "b"} {
		if err := c.EnsureVaultCA(ctx, []byte(pem)); err != nil {
			t.Fatalf("%s: %v", pem, err)
		}
	}
	if cm, err := cs.CoreV1().ConfigMaps("agent-fleet").Get(ctx, VaultCAConfigMap, metav1.GetOptions{}); err != nil || cm.Data["ca.pem"] != "b" {
		t.Fatalf("%v %v", cm, err)
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The definition clone script, EXECUTED against real git (a string assertion
// on a fake clientset proves nothing about a shell script). Fixture: the
// remote advances main and gains a branch after the cache was cloned, the
// cache fetches (as EnsureRepoFetched does), and the cache's LOCAL main stays
// stale. The workspace must start on the remote-tracking tip, with no remote
// contact of its own.
func TestDefinitionCloneScript_RunsAgainstTheCacheOnly(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitT(t, root, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "clone", remote, seed)
	gitT(t, seed, "commit", "--allow-empty", "-m", "a")
	gitT(t, seed, "push", "origin", "main")

	cache := filepath.Join(root, "repo-cache", "dream-analyst")
	gitT(t, root, "clone", remote, cache)
	gitT(t, seed, "commit", "--allow-empty", "-m", "b")
	gitT(t, seed, "push", "origin", "main")
	gitT(t, seed, "checkout", "-b", "feat")
	gitT(t, seed, "commit", "--allow-empty", "-m", "f")
	gitT(t, seed, "push", "origin", "feat")
	gitT(t, cache, "fetch", "origin")
	if gitT(t, cache, "rev-parse", "main") == gitT(t, cache, "rev-parse", "origin/main") {
		t.Fatal("fixture: the cache's local main must be stale")
	}

	for _, base := range []string{"main", "feat"} {
		t.Run(base, func(t *testing.T) {
			spec := definitionSpec()
			spec.BaseBranch = base
			ws := filepath.Join(t.TempDir(), "workspace")
			run := func() {
				if out, err := runCloneScript(t, root, spec, ws); err != nil {
					t.Fatalf("clone script: %v: %s", err, out)
				}
			}
			run()
			if got, want := gitT(t, ws, "rev-parse", "HEAD"), gitT(t, cache, "rev-parse", "origin/"+base); got != want {
				t.Errorf("HEAD %s, want the cache's origin/%s %s", got, base, want)
			}
			// origin/* is the cache's remote-tracking view, not its frozen
			// local branches: no phantom "ahead N" on a clean checkout.
			for _, b := range []string{"main", "feat"} {
				if got, want := gitT(t, ws, "rev-parse", "origin/"+b), gitT(t, cache, "rev-parse", "origin/"+b); got != want {
					t.Errorf("workspace origin/%s %s, want the cache's %s", b, got, want)
				}
			}
			if got := gitT(t, ws, "rev-list", "--count", "origin/"+base+"..HEAD"); got != "0" {
				t.Errorf("HEAD is %s ahead of origin/%s on a fresh clone", got, base)
			}
			if got := gitT(t, ws, "rev-parse", "--abbrev-ref", "HEAD"); got != base {
				t.Errorf("branch %q, want %q", got, base)
			}
			if got := gitT(t, ws, "remote", "get-url", "origin"); got != spec.RepoURL {
				t.Errorf("origin %q, want the real URL", got)
			}
			gitT(t, ws, "status")
			gitT(t, ws, "log", "-1")
			// A warm re-runs it: the finished tree, work included, is left alone.
			if err := os.WriteFile(filepath.Join(ws, "work.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			run()
			if _, err := os.Stat(filepath.Join(ws, "work.txt")); err != nil {
				t.Fatalf("a warm touched a finished workspace: %v", err)
			}
		})
	}

	// A clone that died part-way is never accepted, and the next warm starts
	// over. A run that failed (a missing base) or was killed (OOM, eviction,
	// SIGKILL: no trap runs) leaves a .git with no checkout, or checked-out
	// files, and no marker.
	spec := definitionSpec()
	spec.BaseBranch = "missing"
	ws := filepath.Join(t.TempDir(), "workspace")
	for i := range 2 {
		if out, err := runCloneScript(t, root, spec, ws); err == nil {
			t.Fatalf("run %d: a missing base succeeded: %s", i, out)
		}
	}
	killed := filepath.Join(t.TempDir(), "workspace")
	gitT(t, root, "clone", "--no-checkout", cache, killed)
	if err := os.WriteFile(filepath.Join(killed, "half-written"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.BaseBranch = "main"
	for _, w := range []string{ws, killed} {
		if out, err := runCloneScript(t, root, spec, w); err != nil {
			t.Fatalf("%s: a warm after a partial clone: %v: %s", w, err, out)
		}
		if got, want := gitT(t, w, "rev-parse", "HEAD"), gitT(t, cache, "rev-parse", "origin/main"); got != want {
			t.Errorf("%s: HEAD %s, want %s", w, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(killed, "half-written")); !os.IsNotExist(err) {
		t.Errorf("the partial tree was not wiped: %v", err)
	}
}

// runCloneScript runs spec's definition clone script against root's cache,
// into ws. The script writes global config, kept in a temp HOME, and gets no
// network: a remote contact would hit the real GitHub URL.
func runCloneScript(t *testing.T, root string, spec WorkerPodSpec, ws string) ([]byte, error) {
	t.Helper()
	script := container(t, createJob(t, spec), "clone").Command[2]
	script = strings.ReplaceAll(script, "/repo-cache", filepath.Join(root, "repo-cache"))
	script = strings.ReplaceAll(script, sessionWorkdir, ws)
	home := t.TempDir()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=file")
	return cmd.CombinedOutput()
}
