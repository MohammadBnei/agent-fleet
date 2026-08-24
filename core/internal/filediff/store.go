// Package filediff is the parking lot between a human clicking a file in the
// CHANGES panel and the sidecar answering with a real `git diff` for it.
//
// It exists because core cannot ask the sidecar anything. The session's working
// tree lives on a ReadWriteOnce PVC that only the session's own pod mounts; the
// provisioner never mounts it and core has no cluster RBAC at all, so the
// sidecar is the sole window onto it — and the only time that window is open is
// the sidecar's own 5s PushToolTelemetry call. A request therefore cannot be
// fetched, only ARMED: the dashboard records a want here, the next telemetry
// tick carries it out, and the one after carries the answer back.
//
// Deliberately in memory, not Postgres. A diff is only meaningful while the pod
// that can produce it is alive, so there is nothing worth persisting: after
// teardown the honest answer is "no pod", which needs no storage to give. Core
// is a single instance, so a restart costs one re-click.
//
// Answers are read ONCE and dropped. That is not an optimisation — it is what
// keeps the console from showing a diff of a working tree that has moved on.
// The PVC survives teardown, so a re-warmed session has the same paths with
// different contents, and a cache with no clock would serve the old one
// indefinitely. Reading once means every open of the modal asks the pod again,
// and it bounds memory without an eviction policy.
package filediff

import "sync"

const (
	// Wants outstanding per session. A human reads one file at a time; this
	// is sized for impatient clicking, not for breadth.
	maxWantedPerSession = 16
	// Answers held for a session that stopped polling (modal closed, tab
	// gone). Small on purpose: an uncollected answer is garbage by
	// definition, and this is the only thing holding it.
	maxReadyPerSession = 16
	// Sessions tracked at once, against MAX_LIVE_SESSIONS of 5. A backstop
	// for the leak shape this type has — a map keyed by session id, fed by
	// pods, with no lifecycle event telling it a session is over.
	maxSessions = 32
	// One diff's size cap. A lockfile rewrite or a generated file is exactly
	// what lands here, and an unbounded string arriving over gRPC from a pod
	// is a memory bug with a network interface.
	MaxDiffBytes = 256 * 1024
)

// Store is safe for concurrent use: the dashboard handler and the sidecar's
// telemetry call reach it from two different gRPC servers.
type Store struct {
	mu     sync.Mutex
	wanted map[string]map[string]struct{} // sessionID -> path
	ready  map[string]map[string]string   // sessionID -> path -> diff
}

func New() *Store {
	return &Store{wanted: map[string]map[string]struct{}{}, ready: map[string]map[string]string{}}
}

// Want arms a request for path. Returns false when the session is at its cap,
// so the caller can say so rather than dropping the click silently — a dropped
// want is indistinguishable from a slow pod, and the console polls forever.
func (s *Store) Want(sessionID, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.wanted[sessionID]
	if w == nil {
		if len(s.wanted) >= maxSessions {
			evictOne(s.wanted)
		}
		w = map[string]struct{}{}
		s.wanted[sessionID] = w
	}
	if _, ok := w[path]; !ok && len(w) >= maxWantedPerSession {
		return false
	}
	w[path] = struct{}{}
	return true
}

// Take returns the sidecar's answer for path and drops it. See the package
// doc: an answer is read once, so reopening the file asks the pod again.
func (s *Store) Take(sessionID, path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.ready[sessionID]
	diff, ok := r[path]
	if ok {
		delete(r, path)
		if len(r) == 0 {
			delete(s.ready, sessionID)
		}
	}
	return diff, ok
}

// Wanted lists the paths the sidecar should diff on its next tick, and clears
// them. Clearing on read is what stops a path the pod cannot answer (committed
// away, reverted, never tracked) from being re-asked every tick forever: the
// sidecar reports an empty diff for it, which is a real answer and ends the
// poll.
func (s *Store) Wanted(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.wanted[sessionID]
	if len(w) == 0 {
		return nil
	}
	paths := make([]string, 0, len(w))
	for p := range w {
		paths = append(paths, p)
	}
	delete(s.wanted, sessionID)
	return paths
}

// Put files the sidecar's answers. An empty diff is a real answer — git found
// nothing for that path — and is stored as such so the console stops polling.
func (s *Store) Put(sessionID string, diffs map[string]string) {
	if len(diffs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.ready[sessionID]
	if r == nil {
		if len(s.ready) >= maxSessions {
			evictOne(s.ready)
		}
		r = map[string]string{}
		s.ready[sessionID] = r
	}
	for path, diff := range diffs {
		if len(diff) > MaxDiffBytes {
			diff = diff[:MaxDiffBytes] + "\n… diff truncated\n"
		}
		if _, exists := r[path]; !exists && len(r) >= maxReadyPerSession {
			evictOne(r)
		}
		r[path] = diff
	}
}

// evictOne drops an arbitrary entry. Arbitrary is honest here: every caller is
// at a cap that only a misbehaving or abandoned session reaches, and an LRU
// would be a clock and an ordering to maintain for a case that should not
// happen. Generic so both maps use the same one.
func evictOne[V any](m map[string]V) {
	for k := range m {
		delete(m, k)
		return
	}
}
