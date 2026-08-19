//go:build integration

package proposals

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// A proposal is the gate in front of a machine-initiated run: an alert fires
// or an audit ticks, and instead of dispatching an agent at a repo, the fleet
// files something a human has to approve. All of the behaviour worth testing
// here is in a partial unique index and three guarded UPDATEs — SQL, invisible
// to the compiler, and the difference between "one suggestion" and "a
// suggestion every minute for three hours".

func newStore(t *testing.T) (*Store, *sessions.Store, context.Context) {
	t.Helper()
	pool := dbtest.NewPool(t)
	return NewStore(pool), sessions.NewStore(pool), context.Background()
}

// The dedup window is "not dismissed", NOT "not yet opened". Keyed on
// un-opened, the key would free the moment a human clicks approve — so a
// 1-hour audit cadence whose session runs 3 hours files three proposals for
// the same work, and the human who already approved it gets asked twice more.
func TestCreate_DedupHoldsWhileOpenAndAfterApproval(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	id, created, err := store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it")
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	// The audit ticks again while the proposal is still sitting there.
	_, created, err = store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("a second proposal was filed while one was already standing")
	}

	// A human approves it. The key must STAY held — the work is now in
	// progress, and proposing it again would be proposing something someone is
	// already doing.
	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Open(ctx, id, sessionID); err != nil {
		t.Fatalf("open: %v", err)
	}

	_, created, err = store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it")
	if err != nil {
		t.Fatalf("third create: %v", err)
	}
	if created {
		t.Fatal("the dedup key freed on approval — a long-running session would collect " +
			"one duplicate proposal per audit tick for as long as it runs")
	}
}

// Dismissing means "not now", not "never": a still-firing alert should be
// proposed again on its next fire. Permanent suppression is an Alertmanager
// silence, which is where that decision belongs.
func TestDismiss_ReArmsTheDedupKey(t *testing.T) {
	store, _, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "disk-full", "Disk full", "look into it")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Dismiss(ctx, id); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	_, created, err := store.Create(ctx, "agent-fleet", "alert", "disk-full", "Disk full", "look into it")
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if !created {
		t.Fatal("a dismissed proposal still holds its dedup key — the alert can never be " +
			"raised again, which turns 'not now' into 'never'")
	}
}

// Archiving the session an approval produced is what releases the key for the
// next audit tick. Without it the key stays held by a proposal whose session
// is long finished, and the same work is never suggested again.
func TestDismissForSession_ReArmsAfterTheWorkIsFinished(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "audit", "weekly-deps", "Deps stale", "bump them")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Open(ctx, id, sessionID); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := store.DismissForSession(ctx, sessionID); err != nil {
		t.Fatalf("dismiss for session: %v", err)
	}

	_, created, err := store.Create(ctx, "agent-fleet", "audit", "weekly-deps", "Deps stale", "bump them")
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if !created {
		t.Fatal("archiving the session did not re-arm the key — this work can never be proposed again")
	}
}

// Open is the write that hands a cluster-access agent a session. Two clicks,
// two humans, or one stale browser tab must not turn one proposal into two
// sessions — the guarded UPDATE is what makes the second caller lose.
func TestOpen_IsExactlyOnceUnderConcurrentClicks(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "flapping", "Service flapping", "investigate")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const clicks = 6
	sessionIDs := make([]string, clicks)
	for i := range sessionIDs {
		sid, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		sessionIDs[i] = sid
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []string
	start := make(chan struct{})
	for _, sid := range sessionIDs {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			<-start
			err := store.Open(ctx, id, sid)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won = append(won, sid)
			} else if !errors.Is(err, ErrNotOpen) {
				t.Errorf("unexpected error: %v", err)
			}
		}(sid)
	}
	close(start)
	wg.Wait()

	if len(won) != 1 {
		t.Fatalf("%d concurrent approvals succeeded, want exactly 1 — each extra one is a "+
			"second cluster-access agent dispatched from a single human decision", len(won))
	}

	// And a later click on the same proposal is a clean, specific refusal
	// rather than a silent second dispatch.
	err = store.Open(ctx, id, sessionIDs[0])
	if !errors.Is(err, ErrNotOpen) {
		t.Fatalf("re-opening an already-opened proposal returned %v, want ErrNotOpen", err)
	}
}

// A dismissed proposal cannot then be approved. The two are the same decision
// made twice in opposite directions, and the second one must lose whichever
// way round it happens.
func TestOpen_CannotResurrectADismissedProposal(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "k", "T", "B")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Dismiss(ctx, id); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := store.Open(ctx, id, sessionID); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("opening a dismissed proposal returned %v, want ErrNotOpen", err)
	}
	if err := store.Dismiss(ctx, id); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("dismissing twice returned %v, want ErrNotOpen", err)
	}
}

// ListOpen is what the dashboard renders. Anything already decided must not
// come back: an approved proposal is work in flight, a dismissed one is a
// decision already made, and showing either invites a second click on
// something settled.
func TestListOpen_ShowsOnlyWhatIsStillUndecided(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	open, _, err := store.Create(ctx, "agent-fleet", "audit", "a", "still open", "b")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	approved, _, err := store.Create(ctx, "agent-fleet", "audit", "b", "approved", "b")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dismissed, _, err := store.Create(ctx, "agent-fleet", "audit", "c", "dismissed", "b")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Open(ctx, approved, sessionID); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Dismiss(ctx, dismissed); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	list, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != open {
		ids := make([]string, len(list))
		for i, p := range list {
			ids[i] = p.ID
		}
		t.Fatalf("ListOpen returned %v, want exactly [%s]", ids, open)
	}
}

// Deleting a session must not take its proposal with it. The FK is
// ON DELETE SET NULL precisely so the history of "this was suggested and
// approved" survives — and it is half of what let deleted_at/SoftDelete die.
func TestDeletingASessionDetachesRatherThanDestroysItsProposal(t *testing.T) {
	store, sessionStore, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "audit", "k", "T", "B")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Open(ctx, id, sessionID); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := sessionStore.Delete(ctx, sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	p, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p == nil {
		t.Fatal("deleting the session destroyed its proposal — the FK must be SET NULL, not CASCADE")
	}
	if p.SessionID != nil {
		t.Errorf("proposal still points at a deleted session: %v", p.SessionID)
	}
}
