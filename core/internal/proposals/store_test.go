//go:build integration

package proposals

import (
	"context"
	"encoding/json"
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

	id, created, err := store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it", "")
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	// The audit ticks again while the proposal is still sitting there.
	_, created, err = store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it", "")
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

	_, created, err = store.Create(ctx, "agent-fleet", "audit", "nightly-lint", "Lint is failing", "fix it", "")
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

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "disk-full", "Disk full", "look into it", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Dismiss(ctx, id); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	_, created, err := store.Create(ctx, "agent-fleet", "alert", "disk-full", "Disk full", "look into it", "")
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

	id, _, err := store.Create(ctx, "agent-fleet", "audit", "weekly-deps", "Deps stale", "bump them", "")
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

	_, created, err := store.Create(ctx, "agent-fleet", "audit", "weekly-deps", "Deps stale", "bump them", "")
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

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "flapping", "Service flapping", "investigate", "")
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

	id, _, err := store.Create(ctx, "agent-fleet", "alert", "k", "T", "B", "")
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

	open, _, err := store.Create(ctx, "agent-fleet", "audit", "a", "still open", "b", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	approved, _, err := store.Create(ctx, "agent-fleet", "audit", "b", "approved", "b", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dismissed, _, err := store.Create(ctx, "agent-fleet", "audit", "c", "dismissed", "b", "")
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

	id, _, err := store.Create(ctx, "agent-fleet", "audit", "k", "T", "B", "")
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

// The raw alert has to survive the round trip, because `body` is a lossy
// flattening of it and nothing else keeps a copy — core has no Alertmanager
// client to ask again. A JSONB column also normalises what it stores, so
// asserting on the string is not enough: it has to still parse, with the keys
// intact.
func TestCreate_KeepsRawPayloadThroughListAndGet(t *testing.T) {
	store, _, ctx := newStore(t)

	raw := `{"labels":{"alertname":"PodCrashLooping","container":"api"},"annotations":{"runbook_url":"https://runbook/pcl"},"generatorURL":"https://prom/graph?g0.expr=up==0"}`
	id, created, err := store.Create(ctx, "infra-bootstrap", "alert", "fp1", "PodCrashLooping", "flattened text", raw)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	var decoded struct {
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generatorURL"`
	}
	if err := json.Unmarshal([]byte(got.Payload), &decoded); err != nil {
		t.Fatalf("payload did not survive as JSON: %v (%q)", err, got.Payload)
	}
	if decoded.Labels["container"] != "api" || decoded.Annotations["runbook_url"] == "" || decoded.GeneratorURL == "" {
		t.Errorf("payload lost fields: %+v", decoded)
	}

	list, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Payload != got.Payload {
		t.Errorf("ListOpen must return the same payload Get does, got %d rows", len(list))
	}
}

// A schedule files with no payload at all. The column is NOT NULL so that no
// reader has to handle a nil — an empty object, not an empty string and not a
// NULL that fails the whole query rather than the row.
func TestCreate_EmptyPayloadStoredAsEmptyObject(t *testing.T) {
	store, _, ctx := newStore(t)

	id, _, err := store.Create(ctx, "agent-fleet", "schedule", "schedule:x", "Scheduled: x", "do the thing", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Payload != "{}" {
		t.Errorf("empty payload should read back as {}, got %q", got.Payload)
	}
}

// openInto files a proposal and opens it into a fresh session, the shape every
// re-arm case below starts from.
func openInto(t *testing.T, store *Store, sessionStore *sessions.Store, ctx context.Context, key string) (string, string) {
	t.Helper()
	id, created, err := store.Create(ctx, "agent-fleet", "schedule", key, "T", "B", "")
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	sessionID, err := sessionStore.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Open(ctx, id, sessionID); err != nil {
		t.Fatalf("open: %v", err)
	}
	return id, sessionID
}

func refile(t *testing.T, store *Store, ctx context.Context, key string) bool {
	t.Helper()
	_, created, err := store.Create(ctx, "agent-fleet", "schedule", key, "T", "B", "")
	if err != nil {
		t.Fatalf("refile: %v", err)
	}
	return created
}

// The bug this change exists for. The retention GC sweeps a session's disk and
// marks it swept — it is no longer resumable — but nothing dismissed the
// proposal that opened it, so the dedup key stayed held and the schedule
// logged "previous run still open" on every 60s tick, forever, behind a green
// status dot.
func TestCreate_ReArmsOnceTheSessionIsSwept(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	_, sessionID := openInto(t, store, sessionStore, ctx, "schedule:swept")

	if refile(t, store, ctx, "schedule:swept") {
		t.Fatal("the key freed while the session was still resumable")
	}
	if err := sessionStore.MarkSwept(ctx, sessionID); err != nil {
		t.Fatalf("mark swept: %v", err)
	}
	if !refile(t, store, ctx, "schedule:swept") {
		t.Fatal("a swept session still held its dedup key — the schedule that filed it can never run again")
	}
}

// ArchiveSession dismisses the proposal itself, but only best-effort: the call
// is logged and not propagated, so an archive whose dismiss failed used to
// wedge the key permanently with nothing to show for it.
func TestCreate_ReArmsOnceTheSessionIsArchived(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	_, sessionID := openInto(t, store, sessionStore, ctx, "schedule:archived")

	if err := sessionStore.Archive(ctx, sessionID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if !refile(t, store, ctx, "schedule:archived") {
		t.Fatal("an archived session still held its dedup key")
	}
}

// The regression guard for this whole change.
//
// A session whose pod is gone and whose last transcript entry is a `result`
// looks finished, and two separate drafts of the re-arm predicate keyed on
// exactly that. Both were wrong: the pod is reaped by the idle sweep after 30
// minutes and the worker is explicitly paused between rounds, so this is the
// STEADY STATE of every opened session, not an ending. Re-arming here files a
// duplicate proposal for work a human is still in the middle of — and for
// infra-bootstrap that is a second openable cluster-access session.
//
// TestCreate_DedupHoldsWhileOpenAndAfterApproval cannot catch this: its
// session is never warmed, so pod_phase stays NULL and it passes either way.
func TestCreate_HoldsWhileTheSessionIsOnlyPodless(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	_, sessionID := openInto(t, store, sessionStore, ctx, "schedule:cold")

	if _, err := sessionStore.ReserveSlot(ctx, sessionID, 5); err != nil {
		t.Fatalf("reserve slot: %v", err)
	}
	if err := sessionStore.TouchActive(ctx, sessionID, "agent", "result"); err != nil {
		t.Fatalf("touch active: %v", err)
	}
	if err := sessionStore.SetPodPhase(ctx, sessionID, "POD_PHASE_TERMINATED", "idle timeout"); err != nil {
		t.Fatalf("set pod phase: %v", err)
	}

	if refile(t, store, ctx, "schedule:cold") {
		t.Fatal("the key freed on pod death — a session paused between rounds is not a finished run, " +
			"and this files a duplicate for work still in flight")
	}
}

// A crashed pod never writes a `result` entry, so any predicate keyed on one
// would wedge here exactly as the original bug did. It is not archived and not
// swept, though, so the key legitimately stays held until one of those
// happens — the schedule reports which session, rather than stalling silently.
func TestCreate_HoldsAfterACrashButNamesTheHolder(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	_, sessionID := openInto(t, store, sessionStore, ctx, "schedule:crashed")

	if _, err := sessionStore.ReserveSlot(ctx, sessionID, 5); err != nil {
		t.Fatalf("reserve slot: %v", err)
	}
	if err := sessionStore.SetPodPhase(ctx, sessionID, "POD_PHASE_CRASHED", "OOMKilled"); err != nil {
		t.Fatalf("set pod phase: %v", err)
	}
	if refile(t, store, ctx, "schedule:crashed") {
		t.Fatal("the key freed on a crash")
	}

	holder, err := store.StandingFor(ctx, "agent-fleet", "schedule:crashed")
	if err != nil {
		t.Fatalf("standing for: %v", err)
	}
	if holder != sessionID {
		t.Errorf("StandingFor = %q, want the holding session %q — a skipped tick that cannot name "+
			"the holder is the silent stall this change exists to end", holder, sessionID)
	}
}

// Deleting a session detaches its proposal rather than destroying it, so the
// row returns to ListOpen still holding the key. That is deliberate and not a
// wedge: it is visible in the inbox and a human can dismiss it. The reap must
// leave it alone.
func TestCreate_HoldsAfterTheSessionIsDeleted(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	_, sessionID := openInto(t, store, sessionStore, ctx, "schedule:deleted")

	if err := sessionStore.Delete(ctx, sessionID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if refile(t, store, ctx, "schedule:deleted") {
		t.Fatal("the key freed on a session delete, so the detached proposal in the inbox and a fresh " +
			"one now both stand for the same work")
	}
}

// StandingFor is what turns "previous run still open" into something a human
// can act on, so it must not name a session for a proposal nobody has opened.
func TestStandingFor_IsEmptyForAnUnopenedProposal(t *testing.T) {
	store, _, ctx := newStore(t)
	if _, _, err := store.Create(ctx, "agent-fleet", "schedule", "schedule:unopened", "T", "B", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	holder, err := store.StandingFor(ctx, "agent-fleet", "schedule:unopened")
	if err != nil {
		t.Fatalf("standing for: %v", err)
	}
	if holder != "" {
		t.Errorf("StandingFor = %q for a proposal still sitting in the inbox, want \"\"", holder)
	}
}

// The reap and the insert are one transaction. If the dismissal could commit
// while the insert failed, the key would be freed with nothing filed — and the
// dismissed row would carry a reason that never happened, which also turns a
// later DismissForSession into a silent no-op.
//
// Forced through the proposals_source_check CHECK constraint, which is a real
// failure this path can hit rather than an injected one.
func TestCreate_ReapRollsBackWhenTheInsertFails(t *testing.T) {
	store, sessionStore, ctx := newStore(t)
	id, sessionID := openInto(t, store, sessionStore, ctx, "schedule:atomic")
	if err := sessionStore.MarkSwept(ctx, sessionID); err != nil {
		t.Fatalf("mark swept: %v", err)
	}

	// Same key, so the reap fires — but an invalid source, so the insert dies.
	if _, _, err := store.Create(ctx, "agent-fleet", "not-a-valid-source", "schedule:atomic", "T", "B", ""); err == nil {
		t.Fatal("an invalid source was accepted; this test no longer exercises a failing insert")
	}

	p, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p == nil {
		t.Fatal("proposal vanished")
	}
	if err := store.Dismiss(ctx, id); err != nil {
		t.Fatalf("the reap committed despite the insert failing, so the key was freed with nothing "+
			"filed: %v", err)
	}
}
