package coreserver

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"
)

// AccessLogInterceptor logs one line per gRPC call (method, duration, error
// or nil) — this surface had zero request-level logging before, matching
// the same gap the dashboard's connect.Interceptor closes on the HTTP side.
func AccessLogInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	slog.Info("core grpc", "method", info.FullMethod, "duration_ms", time.Since(start).Milliseconds(), "error", errString(err))
	return resp, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
