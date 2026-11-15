//go:build integration

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// slowProvisioner takes its time over every create, as the real one does
// (clone, PVC, Job), and fails the first `fail` creates.
type slowProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	delay time.Duration

	mu                sync.Mutex
	fail              int
	calls             map[string]int
	inFlight, maxLive int
}

func (p *slowProvisioner) CreateWorkerPod(_ context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	p.mu.Lock()
	p.calls[req.GetSessionId()]++
	p.inFlight++
	p.maxLive = max(p.maxLive, p.inFlight)
	failing := p.fail > 0
	if failing {
		p.fail--
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	if failing {
		return nil, errors.New("clone failed")
	}
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-" + req.GetSessionId()}, nil
}

type raceFixture struct {
	ctx      context.Context
	s        *Server
	prov     *slowProvisioner
	sessions *sessions.Store
	transcr  transcript.Store
}

func newRaceFixture(t *testing.T, delay time.Duration, fail int) raceFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	prov := &slowProvisioner{delay: delay, fail: fail, calls: map[string]int{}}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(gs, prov)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	provClient, err := provisionerclient.New(lis.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provClient.Close() })
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('editable-blog', 'https://example.invalid/x.git')`); err != nil {
		t.Fatal(err)
	}
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	s := &Server{sessions: store, repos: repos.NewStore(pool), transcr: transcr, e2e: provClient, caps: sessions.Caps{MaxLive: 5}}
	return raceFixture{ctx: ctx, s: s, prov: prov, sessions: store, transcr: transcr}
}

func (f raceFixture) session(t *testing.T) string {
	t.Helper()
	id, err := f.sessions.Create(f.ctx, sessions.CreateParams{Repo: "editable-blog"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f raceFixture) post(id, text string) error {
	_, err := f.s.PostMessage(f.ctx, connect.NewRequest(&agentfleetv1.PostMessageRequest{SessionId: id, Text: text}))
	return err
}

// humanTexts is the session's human messages in seq order.
func (f raceFixture) humanTexts(t *testing.T, id string) []string {
	t.Helper()
	entries, _, err := f.transcr.ReadSince(f.ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.From == "human" {
			out = append(out, e.Text)
		}
	}
	return out
}

// Concurrent sends to one cold session: one pod, and the messages land in the
// order the sends arrived. Before the send lock, the second send read the
// first one's PROVISIONING reservation as a live pod and appended while the
// first was still inside CreateWorkerPod — ahead of the first message, which
// then sat in a pod's context out of order.
func TestPostMessage_ConcurrentSendsToAColdSessionWarmOnceInOrder(t *testing.T) {
	f := newRaceFixture(t, 300*time.Millisecond, 0)
	id := f.session(t)

	const n = 5
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if err := f.post(id, fmt.Sprintf("m%d", i)); err != nil {
				t.Errorf("post m%d: %v", i, err)
			}
		})
		// Arrival order is the order each send reached the lock.
		waitFor(t, func() bool { return f.s.sends.waiting(id) == i+1 })
	}
	wg.Wait()

	if got := f.prov.count(id); got != 1 {
		t.Errorf("%d CreateWorkerPod calls for one session, want 1", got)
	}
	got := f.humanTexts(t, id)
	want := []string{"m0", "m1", "m2", "m3", "m4"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("transcript order %v, want call order %v", got, want)
	}
}

// Two sessions never wait on each other's provisioner.
func TestPostMessage_TwoSessionsWarmInParallel(t *testing.T) {
	const delay = 500 * time.Millisecond
	f := newRaceFixture(t, delay, 0)
	a, b := f.session(t), f.session(t)

	start := time.Now()
	var wg sync.WaitGroup
	for _, id := range []string{a, b} {
		wg.Go(func() {
			if err := f.post(id, "hello"); err != nil {
				t.Errorf("post %s: %v", id, err)
			}
		})
	}
	wg.Wait()

	if f.prov.peak() != 2 {
		t.Errorf("at most %d creates in flight at once, want 2: one session's warm blocked the other's", f.prov.peak())
	}
	if took := time.Since(start); took >= 2*delay {
		t.Errorf("two warms took %v, as long as two in a row", took)
	}
}

// A send queued behind a failed warm warms again instead of appending onto the
// failed warm's PROVISIONING reservation, which has no pod behind it.
func TestPostMessage_AWaiterBehindAFailedWarmWarmsAgain(t *testing.T) {
	f := newRaceFixture(t, 300*time.Millisecond, 1)
	id := f.session(t)

	errs := make(chan error, 2)
	go func() { errs <- f.post(id, "first") }()
	waitFor(t, func() bool { return f.s.sends.waiting(id) == 1 })
	go func() { errs <- f.post(id, "second") }()
	waitFor(t, func() bool { return f.s.sends.waiting(id) == 2 })

	if err := <-errs; err == nil {
		t.Fatal("the first send succeeded over a failed create")
	}
	if err := <-errs; err != nil {
		t.Fatalf("the waiter's own warm failed: %v", err)
	}
	if got := f.prov.count(id); got != 2 {
		t.Errorf("%d CreateWorkerPod calls, want 2: the waiter appended without warming", got)
	}
	if got := f.humanTexts(t, id); fmt.Sprint(got) != "[second]" {
		t.Errorf("human messages %v, want [second]", got)
	}
}

func (p *slowProvisioner) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

func (p *slowProvisioner) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxLive
}
