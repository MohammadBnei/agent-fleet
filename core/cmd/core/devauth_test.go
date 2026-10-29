package main

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
)

// The dev identity reaches AgentService's handlers only with the gate off, on unary and streaming calls alike
// (WatchDecisions is a stream).
func TestDevInterceptors(t *testing.T) {
	if got := devInterceptors(false); got != nil {
		t.Fatalf("gate on: %d interceptors; want none", len(got))
	}
	ics := devInterceptors(true)
	if len(ics) != 1 {
		t.Fatalf("gate off: %d interceptors; want 1", len(ics))
	}
	check := func(kind string, ctx context.Context) {
		id, ok := auth.FromContext(ctx)
		if !ok || id.Sub != "dev" || !id.InGroup(auth.RequiredGroup) {
			t.Errorf("%s: identity %+v, %v; want the dev admin", kind, id, ok)
		}
	}
	_, _ = ics[0].WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		check("unary", ctx)
		return nil, nil
	})(context.Background(), nil)
	_ = ics[0].WrapStreamingHandler(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		check("stream", ctx)
		return nil
	})(context.Background(), nil)
}

func TestWithDev(t *testing.T) {
	embed := func() []string { return []string{"https://opus.bnei.dev"} }
	if got := withDev(embed, nil)(); !slices.Equal(got, []string{"https://opus.bnei.dev"}) {
		t.Errorf("no dev: %q", got)
	}
	if withDev(nil, nil) != nil {
		t.Error("no embed, no dev: want nil (no island accepted)")
	}
	if got := withDev(nil, []string{"http://127.0.0.1:7410"})(); !slices.Equal(got, []string{"http://127.0.0.1:7410"}) {
		t.Errorf("dev only: %q", got)
	}
	if got := withDev(embed, []string{"http://127.0.0.1:7410"})(); !slices.Equal(got, []string{"http://127.0.0.1:7410", "https://opus.bnei.dev"}) {
		t.Errorf("both: %q", got)
	}
}
