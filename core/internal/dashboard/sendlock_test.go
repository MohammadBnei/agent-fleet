package dashboard

import (
	"context"
	"testing"
	"time"
)

// Waiters get the lock in arrival order, a waiter that gives up holds
// nothing, and another session's lock is never in the way.
func TestSendLocks(t *testing.T) {
	var l sendLocks
	ctx := context.Background()

	_, release, err := l.lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}

	// Another session is free while "a" is held.
	_, releaseB, err := l.lock(ctx, "b")
	if err != nil {
		t.Fatalf("session b waited on session a: %v", err)
	}
	releaseB()

	// A waiter whose ctx ends leaves nothing behind.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := l.lock(cctx, "a"); err == nil {
		t.Fatal("lock returned while held")
	}
	if n := l.waiting("a"); n != 1 {
		t.Fatalf("after a cancelled wait, %d hold or wait, want 1", n)
	}

	const n = 5
	order := make(chan int, n)
	for i := range n {
		go func() {
			_, r, err := l.lock(ctx, "a")
			if err != nil {
				t.Error(err)
				return
			}
			order <- i
			r()
		}()
		waitFor(t, func() bool { return l.waiting("a") == i+2 })
	}
	release()
	for want := range n {
		if got := <-order; got != want {
			t.Fatalf("waiter %d got the lock in place %d", got, want)
		}
	}
	waitFor(t, func() bool { return l.waiting("a") == 0 })
	if len(l.m) != 0 {
		t.Fatalf("%d entries left after every release", len(l.m))
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
