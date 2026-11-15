// Package workerspans turns the timings a worker reports into exported spans
// (docs/adr/0081 §7). The worker has no OpenTelemetry and no network path to a
// collector: its sidecar relays ExportSpans over the CoreService connection,
// and this package is the only place a worker span becomes a real one.
//
// Core's own ukubi-go Tracing exposes no exporter and mints a span only
// through a Tracer, which would give every span core's clock and a parent in
// core's own traces. So each span is built as a finished snapshot
// (tracetest.SpanStub) and handed to a second OTLP exporter pointed at the same
// endpoint.
//
// Trust: the worker is an untrusted writer. Identity (fleet.session.id) comes
// from the lease the auth interceptor verified, never from the payload; payload
// attributes under fleet. or service. are dropped; parent links resolve only
// inside one request and one turn; and everything is capped in memory per
// session (count, bytes, rate). The caps are in-process state, so they assume
// the single core replica k8s/core.yaml pins.
//
// Export is asynchronous: a bounded queue, a short timeout per batch, and a
// drop when the queue is full, so a slow collector never stalls CoreService (the
// sidecar shares that connection with SendMessage and ConsumeGrant).
package workerspans

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Limits bound what one session may make core hold and export.
type Limits struct {
	MaxSpans int     // spans accepted per session, for the life of the process
	MaxBytes int     // estimated bytes accepted per session
	Rate     float64 // spans per second per session
	Burst    int     // token bucket size
	Queue    int     // batches waiting for the exporter
}

// DefaultLimits: a long session runs a few thousand tool calls; one second of
// a runaway loop is cut at Burst.
var DefaultLimits = Limits{MaxSpans: 20000, MaxBytes: 8 << 20, Rate: 50, Burst: 200, Queue: 64}

const (
	maxSessions   = 4096
	maxAttrs      = 16
	maxKeyLen     = 64
	maxValueLen   = 256
	maxNameLen    = 128
	exportTimeout = 5 * time.Second
)

type usage struct {
	spans, bytes int
	tokens       float64
	last         time.Time
}

// Sink queues spans for export. A nil *Sink accepts nothing.
type Sink struct {
	exp sdktrace.SpanExporter
	lim Limits
	res *resource.Resource
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*usage

	q    chan []sdktrace.ReadOnlySpan
	done chan struct{}
}

// New is the sink for the OTLP/HTTP traces URL in full, or nil when endpoint
// is empty (no export, ExportSpans accepts nothing).
func New(endpoint string, lim Limits) (*Sink, error) {
	if endpoint == "" {
		return nil, nil
	}
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithTimeout(exportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
		otlptracehttp.WithProxy(func(*http.Request) (*url.URL, error) { return nil, nil })) // in-cluster: no proxy
	if err != nil {
		return nil, err
	}
	return NewWithExporter(exp, lim), nil
}

// NewWithExporter is a sink over exp (a test's in-memory one); it starts the
// export goroutine. A nil exp gives a nil sink.
func NewWithExporter(exp sdktrace.SpanExporter, lim Limits) *Sink {
	if exp == nil {
		return nil
	}
	s := &Sink{
		exp:      exp,
		lim:      lim,
		res:      resource.NewSchemaless(semconv.ServiceName("agent-fleet-worker")),
		now:      time.Now,
		sessions: map[string]*usage{},
		q:        make(chan []sdktrace.ReadOnlySpan, lim.Queue),
		done:     make(chan struct{}),
	}
	go s.run()
	return s
}

func (s *Sink) run() {
	defer close(s.done)
	for batch := range s.q {
		ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
		if err := s.exp.ExportSpans(ctx, batch); err != nil {
			slog.Warn("worker spans: export failed", "err", err, "spans", len(batch))
		}
		cancel()
	}
}

// Close drains the queue and shuts the exporter down.
func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.drain()
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	_ = s.exp.Shutdown(ctx)
}

// drain stops accepting and waits for the queued batches to be exported.
func (s *Sink) drain() {
	close(s.q)
	<-s.done
}

// TraceID is the trace of one turn of one session: the same on every pod and
// after a restart, because the turn key is the SDK's result uuid or a
// transcript seq, never a counter a pod keeps.
func TraceID(sessionID, turnKey string) trace.TraceID {
	h := sha256.Sum256([]byte(sessionID + "\x00" + turnKey))
	return trace.TraceID(h[:16])
}

func newSpanID() trace.SpanID {
	var id trace.SpanID
	for id == (trace.SpanID{}) {
		_, _ = rand.Read(id[:])
	}
	return id
}

func clip(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

// clean is a span's attributes as the worker may set them: bounded, and never
// naming the session or the service.
func clean(w *agentfleetv1.WorkerSpan) (attrs []attribute.KeyValue, size int) {
	ok := func(k string) bool {
		return k != "" && !strings.HasPrefix(k, "fleet.") && !strings.HasPrefix(k, "service.") && len(attrs) < maxAttrs
	}
	for k, v := range w.GetAttributes() {
		if ok(k) {
			k, v = clip(k, maxKeyLen), clip(v, maxValueLen)
			attrs = append(attrs, attribute.String(k, v))
			size += len(k) + len(v)
		}
	}
	for k, v := range w.GetIntAttributes() {
		if ok(k) {
			k = clip(k, maxKeyLen)
			attrs = append(attrs, attribute.Int64(k, v))
			size += len(k) + 8
		}
	}
	return attrs, size
}

// admit spends one span of session's budget, or reports it is over a cap.
func (s *Sink) admit(session string, size int, now time.Time) bool {
	u := s.sessions[session]
	if u == nil {
		if len(s.sessions) >= maxSessions {
			// ponytail: forget every session's budget at once rather than keep an LRU; a reset only
			// lets a few live sessions spend their caps again. LRU if it ever shows in a profile.
			s.sessions = map[string]*usage{}
		}
		u = &usage{tokens: float64(s.lim.Burst), last: now}
		s.sessions[session] = u
	}
	u.tokens = min(float64(s.lim.Burst), u.tokens+now.Sub(u.last).Seconds()*s.lim.Rate)
	u.last = now
	if u.tokens < 1 || u.spans >= s.lim.MaxSpans || u.bytes+size > s.lim.MaxBytes {
		return false
	}
	u.tokens--
	u.spans++
	u.bytes += size
	return true
}

// Submit queues spans from session (already authenticated) and returns how many
// it kept. Spans over a cap, malformed, or arriving while the queue is full are
// dropped silently: the worker has nothing to do about it.
func (s *Sink) Submit(session string, in []*agentfleetv1.WorkerSpan) int {
	if s == nil || session == "" || len(in) == 0 {
		return 0
	}
	type kept struct {
		w     *agentfleetv1.WorkerSpan
		attrs []attribute.KeyValue
	}
	var keep []kept
	s.mu.Lock()
	now := s.now()
	for _, w := range in {
		if w.GetName() == "" || w.GetLocalId() == "" || w.GetTurnKey() == "" ||
			w.GetStartUnixNano() <= 0 || w.GetEndUnixNano() < w.GetStartUnixNano() {
			continue
		}
		attrs, size := clean(w)
		if s.admit(session, size+len(w.GetName())+64, now) {
			keep = append(keep, kept{w, attrs})
		}
	}
	s.mu.Unlock()
	if len(keep) == 0 {
		return 0
	}

	// Fresh ids for this request. A parent is honoured only when it is in the
	// same request and the same turn, so a worker cannot point a span at
	// another trace or at a span core minted.
	ids := make(map[string]trace.SpanID, len(keep))
	turn := make(map[string]string, len(keep))
	for _, k := range keep {
		ids[k.w.GetLocalId()] = newSpanID()
		turn[k.w.GetLocalId()] = k.w.GetTurnKey()
	}
	batch := make([]sdktrace.ReadOnlySpan, 0, len(keep))
	for _, k := range keep {
		w := k.w
		tid := TraceID(session, w.GetTurnKey())
		stub := tracetest.SpanStub{
			Name:                 clip(w.GetName(), maxNameLen),
			SpanContext:          trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: ids[w.GetLocalId()], TraceFlags: trace.FlagsSampled}),
			SpanKind:             trace.SpanKindInternal,
			StartTime:            time.Unix(0, w.GetStartUnixNano()),
			EndTime:              time.Unix(0, w.GetEndUnixNano()),
			Attributes:           append(k.attrs, attribute.String("fleet.session.id", session)),
			Resource:             s.res,
			InstrumentationScope: instrumentation.Scope{Name: "agent-fleet/worker"},
		}
		if p := w.GetParentLocalId(); p != "" && p != w.GetLocalId() && turn[p] == w.GetTurnKey() {
			stub.Parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: ids[p], TraceFlags: trace.FlagsSampled})
		}
		switch w.GetStatus() {
		case agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_OK:
			stub.Status.Code = codes.Ok
		case agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_ERROR:
			stub.Status.Code = codes.Error
		}
		batch = append(batch, stub.Snapshot())
	}
	select {
	case s.q <- batch:
		return len(batch)
	default:
		return 0 // queue full: a slow collector costs spans, never CoreService latency
	}
}
