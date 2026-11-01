package coreserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/ukubi-go/telemetry"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
)

// healthWith is the health server with a "canceled" service whose Check answers a bare context error, and a Watch
// that ends after one message (or at once, with that error, for "canceled"), so a stream's trailer arrives.
type healthWith struct{ *health.Server }

func (h healthWith) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if req.GetService() == "canceled" {
		return nil, context.Canceled
	}
	return h.Server.Check(ctx, req)
}

func (healthWith) Watch(req *healthpb.HealthCheckRequest, ss grpc.ServerStreamingServer[healthpb.HealthCheckResponse]) error {
	if req.GetService() == "canceled" {
		return context.Canceled
	}
	return ss.Send(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING})
}

// TestAccessLogTraces: a gRPC call on run.go's chain (errkeys, then the access log, inside the ServerOption's span)
// answers with its trace id in one "trace-id" trailer, success or error, unary or stream, and its access log line
// carries that id, the code the client got and the key. gRPC errors are not located: source is "".
func TestAccessLogTraces(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := telemetry.WithExporter(exp, "agent-fleet-core")
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(errkeys.UnaryServerInterceptor(), AccessLogInterceptor),
		grpc.ChainStreamInterceptor(errkeys.StreamServerInterceptor(), StreamAccessLogInterceptor),
		tr.ServerOption(),
	)
	healthpb.RegisterHealthServer(srv, healthWith{health.NewServer()})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := healthpb.NewHealthClient(conn)

	cases := []struct {
		name, service, code, key string
		stream                   bool
	}{
		{"ok", "", "ok", "", false},
		{"unkeyed error", "no-such-service", "not_found", "server.internal", false},
		{"context error", "canceled", "canceled", "server.internal", false},
		{"stream ok", "", "ok", "", true},
		{"stream context error", "canceled", "canceled", "server.internal", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp.Reset()
			logs.Reset()
			var trailer metadata.MD
			req := &healthpb.HealthCheckRequest{Service: c.service}
			var err error
			if c.stream {
				var stream grpc.ServerStreamingClient[healthpb.HealthCheckResponse]
				if stream, err = client.Watch(context.Background(), req, grpc.Trailer(&trailer)); err != nil {
					t.Fatalf("open: %v", err)
				}
				for err == nil {
					_, err = stream.Recv()
				}
				if errors.Is(err, io.EOF) {
					err = nil
				}
			} else {
				_, err = client.Check(context.Background(), req, grpc.Trailer(&trailer))
			}
			got := "ok"
			if err != nil {
				got = connect.Code(status.Code(err)).String()
			}
			if got != c.code {
				t.Fatalf("client got %s (%v), want %s", got, err, c.code)
			}
			if err := tr.Flush(); err != nil {
				t.Fatal(err)
			}
			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("%d spans, want 1", len(spans))
			}
			id := spans[0].SpanContext.TraceID().String()
			if got := trailer.Get("trace-id"); len(got) != 1 || got[0] != id {
				t.Errorf("trace-id trailer %v, span's trace id %s", got, id)
			}
			var line map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
				t.Fatalf("not one JSON line: %q: %v", logs.String(), err)
			}
			if line["msg"] != "core grpc" || line["level"] != "INFO" || line["trace_id"] != id ||
				line["code"] != c.code || line["key"] != c.key || line["source"] != "" {
				t.Errorf("line %v: want core grpc, INFO, trace_id %s, code %s, key %q, source \"\"", line, id, c.code, c.key)
			}
		})
	}
}
