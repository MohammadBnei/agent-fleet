package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newDefinitionsOrigin builds a monorepo-shaped origin with definitions/.
func newDefinitionsOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	for p, c := range map[string]string{
		"definitions/a.yaml":         "a",
		"definitions/clients/b.yaml": "b",
		"README.md":                  "not a definition",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "init")
	return dir
}

func commitFile(t *testing.T, dir, p, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "change "+p)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

func TestReadDefinitions_ReadsFilesAndSHA(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	origin := newDefinitionsOrigin(t)

	files, sha, err := m.ReadDefinitions(context.Background(), origin, "main")
	if err != nil {
		t.Fatal(err)
	}
	if sha != gitIn(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("sha %q is not the origin's HEAD", sha)
	}
	// Plus packs/.listed.md: this provisioner read packs/ (docs/adr/0063).
	if _, listed := files["packs/.listed.md"]; len(files) != 3 || !listed || string(files["definitions/a.yaml"]) != "a" || string(files["definitions/clients/b.yaml"]) != "b" {
		t.Fatalf("files: %v", files)
	}
	// Its own clone, never fleet-shared's: a slow fetch here must not stall
	// CreateWorkerPod's SyncFleetShared.
	if _, err := os.Stat(filepath.Join(root, "definitions-src", ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "fleet-shared")); !os.IsNotExist(err) {
		t.Fatalf("must not touch <root>/fleet-shared: %v", err)
	}

	// A second read refreshes the existing clone.
	want := commitFile(t, origin, "definitions/c.yaml", "c")
	files, sha, err = m.ReadDefinitions(context.Background(), origin, "main")
	if err != nil || sha != want || string(files["definitions/c.yaml"]) != "c" {
		t.Fatalf("refresh: sha %q want %q, files %v, err %v", sha, want, files, err)
	}

	// docs/adr/0063: reviewed packs are read from the same commit.
	want = commitFile(t, origin, "packs/fleet/PACK.md", "fleet rules")
	files, sha, err = m.ReadDefinitions(context.Background(), origin, "main")
	if err != nil || sha != want || string(files["packs/fleet/PACK.md"]) != "fleet rules" {
		t.Fatalf("packs: sha %q want %q, files %v, err %v", sha, want, files, err)
	}
	// A bad pack entry never fails the definitions read: it becomes that
	// pack's .invalid.md, and the rest is read as usual.
	commitFile(t, origin, "packs/repo/huge.md", strings.Repeat("x", maxDefinitionFile+1))
	files, _, err = m.ReadDefinitions(context.Background(), origin, "main")
	if err != nil || !strings.Contains(string(files["packs/repo/.invalid.md"]), "over") || string(files["packs/fleet/PACK.md"]) != "fleet rules" {
		t.Fatalf("bad pack entry: files %v, err %v", files, err)
	}
}

// At a tag, the tagged tree is read, not the branch tip: that is what keeps
// definitions released together with the core that judges them.
func TestReadDefinitions_TagReadsTheTaggedTree(t *testing.T) {
	m := NewManager(t.TempDir())
	origin := newDefinitionsOrigin(t)
	tagged := gitIn(t, origin, "rev-parse", "HEAD")
	gitIn(t, origin, "tag", "4.19.0")
	commitFile(t, origin, "definitions/after-tag.yaml", "x")

	files, sha, err := m.ReadDefinitions(context.Background(), origin, "4.19.0")
	if err != nil {
		t.Fatal(err)
	}
	if sha != tagged {
		t.Fatalf("sha %q, want the tag's %q", sha, tagged)
	}
	if _, ok := files["definitions/after-tag.yaml"]; ok {
		t.Fatal("read the branch tip, not the tag")
	}

	// A later tag on an existing clone moves to that tag.
	next := commitFile(t, origin, "definitions/d.yaml", "d")
	gitIn(t, origin, "tag", "4.20.0")
	_, sha, err = m.ReadDefinitions(context.Background(), origin, "4.20.0")
	if err != nil || sha != next {
		t.Fatalf("sha %q want %q err %v", sha, next, err)
	}

	// At its tag the clone is never refetched (core asks every 60s): with
	// the origin gone, the same tag still reads; a branch has to fetch.
	if err := os.Rename(origin, origin+"-gone"); err != nil {
		t.Fatal(err)
	}
	files, sha, err = m.ReadDefinitions(context.Background(), origin, "4.20.0")
	if err != nil || sha != next || string(files["definitions/d.yaml"]) != "d" {
		t.Fatalf("at the tag with no origin: sha %q err %v", sha, err)
	}
	if _, _, err := m.ReadDefinitions(context.Background(), origin, "main"); err == nil {
		t.Fatal("a branch ref must fetch, and the origin is gone")
	}
}

// The returned content is the commit's, never the checkout's: at its tag the
// clone is not refreshed, so an edited or added file on the PVC would
// otherwise be pinned under the tagged sha.
func TestReadDefinitions_ReadsTheCommitNotTheWorkingTree(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	origin := newDefinitionsOrigin(t)
	gitIn(t, origin, "tag", "4.19.0")
	if _, _, err := m.ReadDefinitions(context.Background(), origin, "4.19.0"); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "definitions-src", "definitions")
	if err := os.WriteFile(filepath.Join(src, "a.yaml"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "planted.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _, err := m.ReadDefinitions(context.Background(), origin, "4.19.0")
	if err != nil {
		t.Fatal(err)
	}
	if string(files["definitions/a.yaml"]) != "a" || len(files) != 3 {
		t.Fatalf("read the working tree: %v", files)
	}
}

// Fail closed: every one of these is an error, never an empty list.
func TestReadDefinitions_FailsClosed(t *testing.T) {
	t.Run("no definitions directory", func(t *testing.T) {
		origin := newOriginRepo(t)
		if _, _, err := NewManager(t.TempDir()).ReadDefinitions(context.Background(), origin, "main"); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("definitions is a file", func(t *testing.T) {
		origin := newOriginRepo(t)
		commitFile(t, origin, "definitions", "x")
		_, _, err := NewManager(t.TempDir()).ReadDefinitions(context.Background(), origin, "main")
		if err == nil || !strings.Contains(err.Error(), "is not a directory") || strings.Contains(err.Error(), "<nil>") {
			t.Fatalf("want a not-a-directory error with no <nil>, got %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		origin := newDefinitionsOrigin(t)
		if err := os.Symlink("/etc/passwd", filepath.Join(origin, "definitions", "evil.yaml")); err != nil {
			t.Fatal(err)
		}
		gitIn(t, origin, "add", ".")
		gitIn(t, origin, "commit", "-m", "symlink")
		_, _, err := NewManager(t.TempDir()).ReadDefinitions(context.Background(), origin, "main")
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("want a symlink error, got %v", err)
		}
	})
	t.Run("oversize file", func(t *testing.T) {
		origin := newDefinitionsOrigin(t)
		commitFile(t, origin, "definitions/big.yaml", strings.Repeat("x", maxDefinitionFile+1))
		_, _, err := NewManager(t.TempDir()).ReadDefinitions(context.Background(), origin, "main")
		if err == nil || !strings.Contains(err.Error(), "over") {
			t.Fatalf("want a size error, got %v", err)
		}
	})
	t.Run("unknown ref", func(t *testing.T) {
		origin := newDefinitionsOrigin(t)
		if _, _, err := NewManager(t.TempDir()).ReadDefinitions(context.Background(), origin, "no-such-ref"); err == nil {
			t.Fatal("want an error")
		}
	})
}

// A provisioner killed mid-git leaves locks or a half-built .git on the PVC,
// and every later refresh failed on them until someone cleaned up by hand
// (2026-10-20: definitions sync down ~2h after the 6.0.2 rollout). The clone
// is disposable: a failed refresh re-clones from scratch.
func TestReadDefinitions_RecoversFromALeftoverLockOrBrokenClone(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	origin := newDefinitionsOrigin(t)
	ctx := context.Background()
	if _, _, err := m.ReadDefinitions(ctx, origin, "main"); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, "definitions-src")

	for _, leftover := range []string{".git/index.lock", ".git/HEAD.lock", ".git/refs/heads/main.lock"} {
		if err := os.WriteFile(filepath.Join(clone, leftover), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		want := commitFile(t, origin, "definitions/c.yaml", leftover)
		files, sha, err := m.ReadDefinitions(ctx, origin, "main")
		if err != nil || sha != want || string(files["definitions/c.yaml"]) != leftover {
			t.Fatalf("%s left behind: sha %q want %q, err %v", leftover, sha, want, err)
		}
	}

	// A clone killed partway: .git exists but is not a repository.
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ReadDefinitions(ctx, origin, "main"); err != nil {
		t.Fatalf("half-built clone not recovered: %v", err)
	}
}

// A caller whose deadline passes mid-refresh must not kill git: the refresh
// finishes on its own bound, leaving a ready clone and no lock.
func TestRefreshDetached_OutlivesACancelledCaller(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	origin := newDefinitionsOrigin(t)
	clone := filepath.Join(root, "definitions-src")
	if _, _, err := m.ReadDefinitions(context.Background(), origin, "main"); err != nil {
		t.Fatal(err)
	}
	want := commitFile(t, origin, "definitions/c.yaml", "c")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.refreshDetached(ctx, clone, origin, "main"); err != nil {
		t.Fatalf("refresh aborted with its caller: %v", err)
	}
	if got := gitIn(t, clone, "rev-parse", "HEAD"); got != want {
		t.Fatalf("clone at %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("index.lock left behind: %v", err)
	}
}

// A caller that already left starts no git at all.
func TestReadDefinitions_DeadCallerRunsNoGit(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.ReadDefinitions(ctx, newDefinitionsOrigin(t), "main"); err == nil {
		t.Fatal("want ctx error")
	}
	if _, err := os.Stat(filepath.Join(root, "definitions-src")); !os.IsNotExist(err) {
		t.Fatalf("a dead caller started a clone: %v", err)
	}
}

// docs/adr/0075 §9: App credentials ride only on clone/fetch, as env; an
// error falls back to ambient git auth instead of failing the call.
func TestGitEnv_OnlyNetworkCommandsAndFallsBack(t *testing.T) {
	m := NewManager(t.TempDir())
	var calls []string
	m.GitEnv = func(context.Context) ([]string, error) {
		calls = append(calls, "env")
		return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=fleet.test", "GIT_CONFIG_VALUE_0=1"}, nil
	}
	origin := newDefinitionsOrigin(t)
	if _, _, err := m.ReadDefinitions(context.Background(), origin, "main"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("GitEnv asked %d times for one clone, want 1 (clone/fetch only)", len(calls))
	}
	m.GitEnv = func(context.Context) ([]string, error) { return nil, errors.New("app down") }
	commitFile(t, origin, "definitions/c.yaml", "c")
	if _, _, err := m.ReadDefinitions(context.Background(), origin, "main"); err != nil {
		t.Fatalf("an App error failed the fetch instead of falling back: %v", err)
	}
}
