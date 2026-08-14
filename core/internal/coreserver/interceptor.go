package coreserver

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
)

// AccessLogInterceptor logs one line per gRPC call (method, duration, error
// if non-nil) — this surface had zero request-level logging before, matching
// the same gap the dashboard's connect.Interceptor closes on the HTTP side.
func AccessLogInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	attrs := []any{"method", info.FullMethod, "duration_ms", time.Since(start).Milliseconds()}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	slog.Info("core grpc", attrs...)
	return resp, err
}
