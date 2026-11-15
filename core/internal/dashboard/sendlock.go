package dashboard

import (
	"context"
	"sync"
)

// sendLocks serializes "warm (if needed) + append" per session, so a second
// sender cannot read the first sender's PROVISIONING phase as live and append
// before the first sender's message, and so at most one CreateWorkerPod per
// session is in flight. Different sessions never share a lock, and nothing
// here holds a database connection: a slow provisioner blocks only the
// senders of its own session.
//
// Waiters are served in arrival order: each lock is a one-slot channel, and
// the Go runtime queues blocked senders on a channel first-in, first-out.
//
// ponytail: in-process, because core runs exactly one replica
// (k8s/core.yaml replicaCount: 1). A second replica needs this to become a
// Postgres advisory lock keyed on the session id, taken in a way that does not
// hold a pool connection across the CreateWorkerPod RPC.
type sendLocks struct {
	mu sync.Mutex
	m  map[string]*sendLock
}

type sendLock struct {
	slot chan struct{} // holding the lock = a value in the slot
	refs int           // holders + waiters, guarded by sendLocks.mu
}

// lock waits for sessionID's lock, or until ctx is done. A caller that gives
// up while waiting, or whose ctx ended as it won the slot, holds nothing.
func (l *sendLocks) lock(ctx context.Context, sessionID string) (func(), error) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sendLock{}
	}
	e := l.m[sessionID]
	if e == nil {
		e = &sendLock{slot: make(chan struct{}, 1)}
		l.m[sessionID] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.slot <- struct{}{}:
	case <-ctx.Done():
		l.drop(sessionID, e)
		return nil, ctx.Err()
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			<-e.slot
			l.drop(sessionID, e)
		})
	}
	// select picks at random when both are ready.
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (l *sendLocks) drop(sessionID string, e *sendLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(l.m, sessionID)
	}
}

// waiting is how many callers hold or wait for sessionID's lock (tests).
func (l *sendLocks) waiting(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sessionID]; e != nil {
		return e.refs
	}
	return 0
}
