package coreserver

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// spanAttr runs one unary call inside a recording span and returns the span's
// fleet.session.id ("" when unset).
func spanAttr(t *testing.T, a *Authenticator, ctx context.Context, method string, req any) string {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	sctx, span := tp.Tracer("t").Start(ctx, "call")
	_, _ = a.UnaryInterceptor(sctx, req, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { return nil, nil })
	span.End()
	for _, kv := range rec.Ended()[0].Attributes() {
		if string(kv.Key) == "fleet.session.id" {
			return kv.Value.AsString()
		}
	}
	return ""
}

func TestAuth_TagsTheSpanWithTheLeaseSession(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "l1"}, "ptok")
	req := &agentfleetv1.GetSessionRequest{Id: "s1"}
	const m = "/agentfleet.v1.CoreService/GetSession"

	if got := spanAttr(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "l1"), m, req); got != "s1" {
		t.Fatalf("session call: fleet.session.id = %q, want s1", got)
	}
	if got := spanAttr(t, a, ctxWith(mdProvisionerToken, "ptok"), m, req); got != "" {
		t.Fatalf("provisioner call: fleet.session.id = %q, want none", got)
	}
	if got := spanAttr(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "bad"), m, req); got != "" {
		t.Fatalf("rejected lease: fleet.session.id = %q, want none", got)
	}
}
