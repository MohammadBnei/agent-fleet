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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func definitionSpec() WorkerPodSpec {
	return WorkerPodSpec{
		SessionID: "task-1", Repo: "dream-analyst", RepoURL: "https://github.com/o/dream-analyst.git", BaseBranch: "main",
		LeaseID:    "lease-1",
		Definition: &DefinitionProfile{Name: "dev-dream-analyst", GitAuthorName: "fleet-dev-agent", GitAuthorEmail: "fleet-dev-agent@bnei.dev"},
	}
}

func createJob(t *testing.T, spec WorkerPodSpec) *batchv1.Job {
	t.Helper()
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
	c := newTestClient()
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
// must (the interim model credential is asserted on purpose, so P3 has to
// flip this line consciously when the vault takes it over).
func TestCreateWorkerPod_DefinitionProfile(t *testing.T) {
	job := createJob(t, definitionSpec())

	worker := container(t, job, "worker")
	for _, absent := range []string{"LEASE_ID", "GH_TOKEN"} {
		if _, ok := findEnv(worker.Env, absent); ok {
			t.Errorf("definition worker has %s", absent)
		}
	}
	for name, want := range map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "oauth-token", // INTERIM until P3 (docs/adr/0064)
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
			script := container(t, createJob(t, spec), "clone").Command[2]
			ws := filepath.Join(t.TempDir(), "workspace")
			script = strings.ReplaceAll(script, "/repo-cache", filepath.Join(root, "repo-cache"))
			script = strings.ReplaceAll(script, sessionWorkdir, ws)
			home := t.TempDir()
			run := func() {
				cmd := exec.Command("sh", "-c", script)
				// The script writes global config; keep it in the temp dir. And
				// no network: a remote contact would hit the real GitHub URL.
				cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
					"GIT_ALLOW_PROTOCOL=file")
				if out, err := cmd.CombinedOutput(); err != nil {
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
			run() // a warm re-runs it: the existing tree is left alone
		})
	}
}
