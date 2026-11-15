package workerspans

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func sp(id, parent, turn string) *agentfleetv1.WorkerSpan {
	return &agentfleetv1.WorkerSpan{Name: "n-" + id, LocalId: id, ParentLocalId: parent, TurnKey: turn,
		StartUnixNano: 1_000, EndUnixNano: 2_000}
}

// drain flushes the sink's queue and returns what was exported (not Close:
// the in-memory exporter forgets its spans on Shutdown).
func drain(s *Sink, exp *tracetest.InMemoryExporter) []tracetest.SpanStub {
	s.drain()
	return exp.GetSpans()
}

func attr(st tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range st.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestTraceID_IsAFunctionOfSessionAndTurnOnly(t *testing.T) {
	a := TraceID("s1", "turn-1")
	if a != TraceID("s1", "turn-1") {
		t.Fatal("same inputs must give the same trace id")
	}
	if a == TraceID("s1", "turn-2") || a == TraceID("s2", "turn-1") {
		t.Fatal("different session or turn must give a different trace id")
	}
	if !a.IsValid() {
		t.Fatal("trace id is zero")
	}
	// "a"+"bc" must not collide with "ab"+"c".
	if TraceID("a", "bc") == TraceID("ab", "c") {
		t.Fatal("session/turn boundary is ambiguous")
	}
}

func TestSubmit_IdentityComesFromCoreNotThePayload(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	s := NewWithExporter(exp, DefaultLimits)
	w := sp("a", "", "t1")
	w.Attributes = map[string]string{"fleet.session.id": "someone-else", "service.name": "core", "gen_ai.tool.name": "Bash"}
	w.IntAttributes = map[string]int64{"fleet.x": 1, "gen_ai.usage.input_tokens": 7}
	w.Status = agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_ERROR
	if n := s.Submit("mine", []*agentfleetv1.WorkerSpan{w}); n != 1 {
		t.Fatalf("accepted %d", n)
	}
	got := drain(s, exp)
	if len(got) != 1 {
		t.Fatalf("exported %d", len(got))
	}
	if v, _ := attr(got[0], "fleet.session.id"); v.AsString() != "mine" {
		t.Fatalf("fleet.session.id = %q", v.AsString())
	}
	for _, k := range []string{"service.name", "fleet.x"} {
		if _, ok := attr(got[0], k); ok {
			t.Errorf("payload attribute %q was kept", k)
		}
	}
	if v, ok := attr(got[0], "gen_ai.tool.name"); !ok || v.AsString() != "Bash" {
		t.Error("an ordinary attribute was lost")
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("status = %v", got[0].Status.Code)
	}
	if got[0].SpanContext.TraceID() != TraceID("mine", "t1") {
		t.Error("trace id is not derived from session and turn")
	}
}

func TestSubmit_ParentsResolveOnlyWithinTheRequestAndTurn(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	s := NewWithExporter(exp, DefaultLimits)
	n := s.Submit("s", []*agentfleetv1.WorkerSpan{
		sp("root", "", "t1"),
		sp("child", "root", "t1"),
		sp("other-turn", "root", "t2"),   // parent is in another turn
		sp("dangling", "not-sent", "t1"), // parent is in no batch
		sp("self", "self", "t1"),
	})
	if n != 5 {
		t.Fatalf("accepted %d", n)
	}
	byName := map[string]tracetest.SpanStub{}
	for _, st := range drain(s, exp) {
		byName[st.Name] = st
	}
	root := byName["n-root"]
	if got := byName["n-child"].Parent.SpanID(); got != root.SpanContext.SpanID() {
		t.Errorf("child parent = %v, want root %v", got, root.SpanContext.SpanID())
	}
	for _, name := range []string{"n-other-turn", "n-dangling", "n-self", "n-root"} {
		if byName[name].Parent.SpanID().IsValid() {
			t.Errorf("%s must be a root", name)
		}
	}
	if !root.SpanContext.SpanID().IsValid() {
		t.Error("span ids must be fresh, not the worker's")
	}
}

func TestSubmit_DropsMalformedSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	s := NewWithExporter(exp, DefaultLimits)
	bad := []*agentfleetv1.WorkerSpan{
		{LocalId: "a", TurnKey: "t", StartUnixNano: 1, EndUnixNano: 2},            // no name
		{Name: "x", TurnKey: "t", StartUnixNano: 1, EndUnixNano: 2},               // no id
		{Name: "x", LocalId: "a", StartUnixNano: 1, EndUnixNano: 2},               // no turn key
		{Name: "x", LocalId: "a", TurnKey: "t", StartUnixNano: 5, EndUnixNano: 2}, // ends before it starts
		{Name: "x", LocalId: "a", TurnKey: "t", StartUnixNano: 0, EndUnixNano: 2}, // no start
	}
	if n := s.Submit("s", bad); n != 0 {
		t.Fatalf("accepted %d malformed spans", n)
	}
	if got := drain(s, exp); len(got) != 0 {
		t.Fatalf("exported %d", len(got))
	}
}

func TestSubmit_CountAndByteCapsAreAPerSessionTotal(t *testing.T) {
	lim := Limits{MaxSpans: 3, MaxBytes: 1 << 20, Rate: 1000, Burst: 1000, Queue: 8}
	exp := tracetest.NewInMemoryExporter()
	s := NewWithExporter(exp, lim)
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{sp("1", "", "t"), sp("2", "", "t")}); n != 2 {
		t.Fatalf("first: %d", n)
	}
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{sp("3", "", "t"), sp("4", "", "t")}); n != 1 {
		t.Fatalf("second: %d, want 1 (cap of 3)", n)
	}
	if n := s.Submit("other", []*agentfleetv1.WorkerSpan{sp("1", "", "t")}); n != 1 {
		t.Fatalf("another session must have its own budget, got %d", n)
	}
	s.Close()

	byteLim := Limits{MaxSpans: 100, MaxBytes: 200, Rate: 1000, Burst: 1000, Queue: 8}
	s = NewWithExporter(tracetest.NewInMemoryExporter(), byteLim)
	big := sp("1", "", "t")
	big.Attributes = map[string]string{"k": string(make([]byte, 250))}
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{big}); n != 0 {
		t.Fatalf("a span over the byte cap was accepted (%d)", n)
	}
	s.Close()
}

func TestSubmit_RateLimitRefillsWithTime(t *testing.T) {
	lim := Limits{MaxSpans: 1000, MaxBytes: 1 << 20, Rate: 10, Burst: 2, Queue: 8}
	s := NewWithExporter(tracetest.NewInMemoryExporter(), lim)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	three := func() []*agentfleetv1.WorkerSpan {
		return []*agentfleetv1.WorkerSpan{sp("1", "", "t"), sp("2", "", "t"), sp("3", "", "t")}
	}
	if n := s.Submit("s", three()); n != 2 {
		t.Fatalf("burst: accepted %d, want 2", n)
	}
	if n := s.Submit("s", three()); n != 0 {
		t.Fatalf("no time passed: accepted %d, want 0", n)
	}
	now = now.Add(100 * time.Millisecond) // 10/s -> one token
	if n := s.Submit("s", three()); n != 1 {
		t.Fatalf("after 100ms: accepted %d, want 1", n)
	}
	s.Close()
}

// blockedExporter holds every export until released, so the queue fills.
type blockedExporter struct {
	release chan struct{}
	started chan struct{}
}

func (b *blockedExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}
func (b *blockedExporter) Shutdown(context.Context) error { return nil }

func TestSubmit_ASlowCollectorDropsInsteadOfBlocking(t *testing.T) {
	b := &blockedExporter{release: make(chan struct{}), started: make(chan struct{}, 1)}
	lim := DefaultLimits
	lim.Queue = 1
	s := NewWithExporter(b, lim)
	s.Submit("s", []*agentfleetv1.WorkerSpan{sp("1", "", "t")})
	<-b.started // the exporter holds batch 1; the queue is empty
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{sp("2", "", "t")}); n != 1 {
		t.Fatalf("queue slot: accepted %d, want 1", n)
	}
	done := make(chan int)
	go func() { done <- s.Submit("s", []*agentfleetv1.WorkerSpan{sp("3", "", "t")}) }()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("a full queue accepted %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked on a slow exporter")
	}
	close(b.release)
	s.Close()
}

func TestNilSinkAcceptsNothing(t *testing.T) {
	var s *Sink
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{sp("1", "", "t")}); n != 0 {
		t.Fatal("nil sink accepted spans")
	}
	s.Close()
	if s2, err := New("", DefaultLimits); s2 != nil || err != nil {
		t.Fatalf("no endpoint: got %v, %v", s2, err)
	}
}
