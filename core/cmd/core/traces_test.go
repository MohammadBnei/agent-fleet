package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/MohammadBnei/ukubi-go/telemetry"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
)

// stopper answers StopSession and StreamTranscript (a server stream, sending nothing) by session id: "fail" is an
// errkeys error built here, "canceled" a bare context error, anything else succeeds. Every other method is the
// generated Unimplemented, an error built without errkeys.
type stopper struct {
	agentfleetv1connect.UnimplementedDashboardServiceHandler
}

func outcome(session string) error {
	switch session {
	case "fail":
		return errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("no such session"))
	case "canceled":
		return context.Canceled
	}
	return nil
}

func (stopper) StopSession(_ context.Context, req *connect.Request[agentfleetv1.StopSessionRequest]) (*connect.Response[agentfleetv1.StopSessionResponse], error) {
	if err := outcome(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.StopSessionResponse{}), nil
}

func (stopper) StreamTranscript(_ context.Context, req *connect.Request[agentfleetv1.StreamTranscriptRequest], _ *connect.ServerStream[agentfleetv1.TranscriptEntry]) error {
	return outcome(req.Msg.GetSessionId())
}

// TestConnectTraces: a call through connectInterceptors, the chain run.go mounts, has one span; its trace id is the
// one Trace-Id response header, success or error, unary or stream (a stream's in its headers, not its trailers), and
// the access log line's trace_id, beside the code the client got, the key and where the error was built. A caller's
// traceparent is a link, never the parent.
func TestConnectTraces(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := telemetry.WithExporter(exp, "agent-fleet-core")
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, h := agentfleetv1connect.NewDashboardServiceHandler(stopper{}, errkeys.WithInterceptors(connectInterceptors(tr, true)...))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := agentfleetv1connect.NewDashboardServiceClient(srv.Client(), srv.URL)

	const inbound = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct {
		name        string
		session     string
		csrf        bool
		traceparent bool
		other       bool // GetSession: Unimplemented
		stream      bool // StreamTranscript
		code, key   string
		source      string // prefix
	}{
		{name: "ok", csrf: true, code: "ok"},
		{name: "ok with a caller's traceparent", csrf: true, traceparent: true, code: "ok"},
		{name: "handler error", session: "fail", csrf: true, code: "not_found", key: "server.internal", source: "cmd/core/traces_test.go:"},
		{name: "csrf refusal", code: "permission_denied", key: "server.internal", source: "internal/dashboard/interceptor.go:"},
		{name: "unkeyed error", other: true, csrf: true, code: "unimplemented", key: "server.internal"},
		{name: "context error", session: "canceled", csrf: true, code: "canceled", key: "server.internal"},
		{name: "stream ok", stream: true, csrf: true, code: "ok"},
		{name: "stream handler error", stream: true, session: "fail", csrf: true, code: "not_found", key: "server.internal", source: "cmd/core/traces_test.go:"},
		{name: "stream context error", stream: true, session: "canceled", csrf: true, code: "canceled", key: "server.internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp.Reset()
			logs.Reset()
			header := func(h http.Header) {
				if c.csrf {
					h.Set("X-Fleet-Dashboard", "1")
				}
				if c.traceparent {
					h.Set("traceparent", "00-"+inbound+"-00f067aa0ba902b7-01")
				}
			}
			var got, trailer http.Header
			var err error
			switch {
			case c.stream:
				req := connect.NewRequest(&agentfleetv1.StreamTranscriptRequest{SessionId: c.session})
				header(req.Header())
				stream, serr := client.StreamTranscript(context.Background(), req)
				if serr != nil {
					t.Fatalf("open: %v", serr)
				}
				for stream.Receive() {
				}
				err, got, trailer = stream.Err(), stream.ResponseHeader(), stream.ResponseTrailer()
				_ = stream.Close()
			case c.other:
				req := connect.NewRequest(&agentfleetv1.GetSessionRequest{})
				header(req.Header())
				_, err = client.GetSession(context.Background(), req)
			default:
				req := connect.NewRequest(&agentfleetv1.StopSessionRequest{SessionId: c.session})
				header(req.Header())
				var resp *connect.Response[agentfleetv1.StopSessionResponse]
				if resp, err = client.StopSession(context.Background(), req); err == nil {
					got = resp.Header()
				}
			}
			if c.code == "ok" && err != nil {
				t.Fatalf("call: %v", err)
			}
			var ce *connect.Error
			if errors.As(err, &ce) {
				if !c.stream {
					got = ce.Meta()
				}
			} else if err != nil {
				t.Fatalf("not a Connect error: %v", err)
			}
			if err != nil && connect.CodeOf(err).String() != c.code {
				t.Errorf("client got %v, want %s", connect.CodeOf(err), c.code)
			}

			if err := tr.Flush(); err != nil {
				t.Fatal(err)
			}
			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("%d spans, want 1", len(spans))
			}
			span := spans[0]
			id := span.SpanContext.TraceID().String()
			if h := got.Values("Trace-Id"); len(h) != 1 || h[0] != id {
				t.Errorf("Trace-Id %q, span's trace id %q", h, id)
			}
			if h := trailer.Values("Trace-Id"); len(h) != 0 {
				t.Errorf("Trace-Id trailer %q, want none", h)
			}
			if c.traceparent {
				if span.Parent.IsValid() || id == inbound {
					t.Errorf("the caller's traceparent is the parent (trace %s)", id)
				}
				if len(span.Links) != 1 || span.Links[0].SpanContext.TraceID().String() != inbound {
					t.Errorf("links %+v, want the caller's trace %s", span.Links, inbound)
				}
			}

			var line map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
				t.Fatalf("not one JSON line: %q: %v", logs.String(), err)
			}
			if line["msg"] != "dashboard rpc" || line["level"] != "INFO" || line["trace_id"] != id {
				t.Errorf("line %v: want msg dashboard rpc, INFO, trace_id %s", line, id)
			}
			if line["code"] != c.code || line["key"] != c.key {
				t.Errorf("code %v key %v, want %s %q", line["code"], line["key"], c.code, c.key)
			}
			src, _ := line["source"].(string)
			if (c.source == "") != (src == "") || !strings.HasPrefix(src, c.source) {
				t.Errorf("source %q, want prefix %q", src, c.source)
			}
		})
	}
}

// TestTracingWithoutEndpoint: with no OTEL_EXPORTER_OTLP_TRACES_ENDPOINT core still mints spans (docs/adr/0081 §4,
// amended), so a call answers with a Trace-Id; a malformed endpoint is refused.
func TestTracingWithoutEndpoint(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		ids, ok  bool
	}{
		{"", true, true},
		{"tempo:4318", false, false},
	} {
		tr, err := newTracing(c.endpoint)
		if (err == nil) != c.ok {
			t.Fatalf("%q: err %v, want ok %v", c.endpoint, err, c.ok)
		}
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = tr.Shutdown() })
		_, h := agentfleetv1connect.NewDashboardServiceHandler(stopper{}, errkeys.WithInterceptors(connectInterceptors(tr, false)...))
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		resp, err := agentfleetv1connect.NewDashboardServiceClient(srv.Client(), srv.URL).
			StopSession(context.Background(), connect.NewRequest(&agentfleetv1.StopSessionRequest{}))
		if err != nil {
			t.Fatalf("%q: %v", c.endpoint, err)
		}
		if got := resp.Header().Get("Trace-Id"); (got != "") != c.ids {
			t.Errorf("%q: Trace-Id %q, want one: %v", c.endpoint, got, c.ids)
		}
	}
}
