package dashboard

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func TestCSRFInterceptor_WrapUnary(t *testing.T) {
	interceptor := NewCSRFInterceptor()
	called := false
	stub := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&agentfleetv1.ApproveResponse{Status: "ok"}), nil
	})
	wrapped := interceptor.WrapUnary(stub)

	t.Run("missing header is rejected", func(t *testing.T) {
		called = false
		req := connect.NewRequest(&agentfleetv1.ApproveRequest{})
		_, err := wrapped(context.Background(), req)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("code = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
		if called {
			t.Error("handler ran without the required header")
		}
	})

	t.Run("present header passes through", func(t *testing.T) {
		called = false
		req := connect.NewRequest(&agentfleetv1.ApproveRequest{})
		req.Header().Set(dashboardHeader, "1")
		if _, err := wrapped(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Error("handler did not run despite the required header")
		}
	})
}

// fakeStreamingHandlerConn is the minimal connect.StreamingHandlerConn
// needed to exercise the interceptor's header check — Receive/Send/
// Response* are never called by csrfInterceptor.
type fakeStreamingHandlerConn struct {
	reqHeader http.Header
}

func (f *fakeStreamingHandlerConn) Spec() connect.Spec          { return connect.Spec{} }
func (f *fakeStreamingHandlerConn) Peer() connect.Peer          { return connect.Peer{} }
func (f *fakeStreamingHandlerConn) Receive(any) error           { return nil }
func (f *fakeStreamingHandlerConn) RequestHeader() http.Header  { return f.reqHeader }
func (f *fakeStreamingHandlerConn) Send(any) error              { return nil }
func (f *fakeStreamingHandlerConn) ResponseHeader() http.Header { return http.Header{} }
func (f *fakeStreamingHandlerConn) ResponseTrailer() http.Header {
	return http.Header{}
}

func TestCSRFInterceptor_WrapStreamingHandler(t *testing.T) {
	interceptor := NewCSRFInterceptor()
	called := false
	stub := connect.StreamingHandlerFunc(func(context.Context, connect.StreamingHandlerConn) error {
		called = true
		return nil
	})
	wrapped := interceptor.WrapStreamingHandler(stub)

	t.Run("missing header is rejected", func(t *testing.T) {
		called = false
		conn := &fakeStreamingHandlerConn{reqHeader: http.Header{}}
		err := wrapped(context.Background(), conn)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("code = %v, want CodePermissionDenied", connect.CodeOf(err))
		}
		if called {
			t.Error("handler ran without the required header")
		}
	})

	t.Run("present header passes through", func(t *testing.T) {
		called = false
		h := http.Header{}
		h.Set(dashboardHeader, "1")
		conn := &fakeStreamingHandlerConn{reqHeader: h}
		if err := wrapped(context.Background(), conn); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Error("handler did not run despite the required header")
		}
	})
}
