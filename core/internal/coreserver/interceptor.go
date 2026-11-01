package coreserver

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
)

// AccessLogInterceptor logs one line per gRPC call (method, duration, error
// if non-nil) — this surface had zero request-level logging before, matching
// the same gap the dashboard's connect.Interceptor closes on the HTTP side.
//
// Like the dashboard's, it answers every call with its trace id (docs/adr/0081 §4), here the "trace-id" trailer.
// The span is the server's StatsHandler's (telemetry's ServerOption), so it exists before any interceptor runs.
func AccessLogInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	if id := errkeys.TraceID(ctx); id != "" {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(traceIDTrailer, id))
	}
	resp, err := handler(ctx, req)
	logCall(ctx, info.FullMethod, start, err)
	return resp, err
}

// StreamAccessLogInterceptor is AccessLogInterceptor for a stream: one line when it ends, the trailer on it.
func StreamAccessLogInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	if id := errkeys.TraceID(ss.Context()); id != "" {
		ss.SetTrailer(metadata.Pairs(traceIDTrailer, id))
	}
	err := handler(srv, ss)
	logCall(ss.Context(), info.FullMethod, start, err)
	return err
}

const traceIDTrailer = "trace-id"

// logCall is the line, with the dashboard's fields: code in Connect's words (the codes are gRPC's numbers), the one
// gRPC sends (a plain error's is its context error's, else Unknown), key the wire's (errkeys.WireKey), source ""
// (gRPC errors are not located).
func logCall(ctx context.Context, method string, start time.Time, err error) {
	attrs := []any{"method", method, "duration_ms", time.Since(start).Milliseconds()}
	code, key := "ok", ""
	if err != nil {
		s, ok := status.FromError(err)
		if !ok {
			s = status.FromContextError(err)
		}
		code, key = connect.Code(s.Code()).String(), string(errkeys.WireKey(err))
		attrs = append(attrs, "error", err.Error())
	}
	attrs = append(attrs, "code", code, "key", key, "source", errkeys.LocationOf(err))
	slog.InfoContext(ctx, "core grpc", attrs...)
}
