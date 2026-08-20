package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// newOriginRepo builds a real local git repo to clone from — same "real temp
// git repo, no mocking" convention the grpcserver tests use.
func newOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "README.md"}, {"commit", "-m", "init"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// The pod-creation path must not wait behind a dashboard sync of the same
// repo. The lock is a plain sync.Mutex, so a wait there ignores the caller's
// context entirely and can outlive it — which is how a 10-minute SyncRepoCache
// used to make a CreateWorkerPod on the same repo miss its own 2-minute
// deadline and report the session CRASHED.
//
// Skipping is only correct once a usable cache exists; the pod's init
// container re-fetches its base branch from the real remote anyway.
func TestEnsureRepoCachedForSession_DoesNotWaitOnAWarmCache(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	origin := newOriginRepo(t)

	if _, err := m.EnsureRepoCloned(ctx, "warm", origin); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Stand in for a sync in flight.
	lock := m.repoLock("warm")
	lock.Lock()
	defer lock.Unlock()

	done := make(chan error, 1)
	go func() { done <- m.EnsureRepoCachedForSession(ctx, "warm", origin) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRepoCachedForSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked behind the held lock — a session would now be waiting out someone else's sync")
	}
}

// The other half: with no cache yet there is nothing to `git clone --shared`
// from, so this one MUST wait however long the holder takes. Skipping here
// would hand the pod an empty /repo-cache directory.
func TestEnsureRepoCachedForSession_WaitsWhenThereIsNoCacheYet(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	origin := newOriginRepo(t)

	lock := m.repoLock("cold")
	lock.Lock()

	done := make(chan error, 1)
	go func() { done <- m.EnsureRepoCachedForSession(ctx, "cold", origin) }()

	select {
	case err := <-done:
		t.Fatalf("returned while the lock was held and no cache existed: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	lock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRepoCachedForSession: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("never completed after the lock was released")
	}
	if _, err := os.Stat(filepath.Join(m.repoPath("cold"), ".git")); err != nil {
		t.Fatalf("expected the cache to have been cloned: %v", err)
	}
}
