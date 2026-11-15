// Package spans times the steps of a warm (CreateWorkerPod) as OTel child
// spans, so Tempo shows where a slow warm spent its time, and keeps the same
// durations in a per-call recorder so one log line can show them without Tempo.
//
// No global TracerProvider is set anywhere in the process (ukubi-go's
// telemetry.Tracing keeps its provider private and only hands it to the gRPC
// server option), so the tracer comes from the span already in ctx: the gRPC
// server span's own provider. A ctx with no span gets the no-op provider, so
// every helper here is safe, and free, in tests and with tracing off.
package spans

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type recKey struct{}

type recorder struct {
	mu    sync.Mutex
	steps []string
}

// WithRecorder returns a ctx whose spans also record "name=<ms>ms" for Summary.
func WithRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, recKey{}, &recorder{})
}

// Summary lists the recorded steps in the order they ended.
func Summary(ctx context.Context) string {
	r, _ := ctx.Value(recKey{}).(*recorder)
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, " ")
}

// Start opens a child span of ctx's span. The returned func ends it, marking
// it failed when err is non-nil.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, sp := trace.SpanFromContext(ctx).TracerProvider().Tracer("agent-fleet-provisioner").Start(ctx, name, trace.WithAttributes(attrs...))
	began := time.Now()
	return ctx, func(err error) {
		if err != nil {
			red := errors.New(Redact(err.Error()))
			sp.RecordError(red)
			sp.SetStatus(codes.Error, red.Error())
		}
		if r, _ := ctx.Value(recKey{}).(*recorder); r != nil {
			r.mu.Lock()
			r.steps = append(r.steps, fmt.Sprintf("%s=%dms", name, time.Since(began).Milliseconds()))
			r.mu.Unlock()
		}
		sp.End()
	}
}

var (
	urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]*@`)
	tokenLike   = regexp.MustCompile(`(?i)\b(?:ghs_|ghp_|gho_|ghu_|ghr_|github_pat_|av_sess_)[A-Za-z0-9_]+|\bBearer\s+\S+`)
)

// Redact removes what an error string must not carry into a trace: URL
// credentials (command argv and stderr quote the repo URL) and token-looking values.
func Redact(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}")
	return tokenLike.ReplaceAllString(s, "[redacted]")
}

// Do runs fn inside a span named name.
func Do(ctx context.Context, name string, fn func(context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, end := Start(ctx, name, attrs...)
	err := fn(ctx)
	end(err)
	return err
}
