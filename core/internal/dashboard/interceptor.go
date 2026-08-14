package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// dashboardHeader is required on every dashboard call, unary and
// streaming alike. BasicAuth credentials are auto-attached by browsers to
// same-origin requests regardless of which page triggered them, so
// BasicAuth alone lets a third-party page forge these calls. A plain HTML
// form/img can't set a custom header, and a cross-origin fetch that tries
// one triggers a CORS preflight this server never allows — so only
// same-origin JS (the dashboard's own SPA) can successfully call these
// routes (see docs/adr/0014, docs/adr/0015).
const dashboardHeader = "X-Fleet-Dashboard"

// csrfInterceptor enforces dashboardHeader on every incoming call to this
// service. core is never its own client here, so
// WrapStreamingClient is a passthrough.
type csrfInterceptor struct{}

func NewCSRFInterceptor() connect.Interceptor {
	return csrfInterceptor{}
}

func (csrfInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Header().Get(dashboardHeader) == "" {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("missing required header"))
		}
		return next(ctx, req)
	}
}

func (csrfInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (csrfInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if conn.RequestHeader().Get(dashboardHeader) == "" {
			return connect.NewError(connect.CodePermissionDenied, errors.New("missing required header"))
		}
		return next(ctx, conn)
	}
}

// accessLogInterceptor logs one line per RPC (method, duration, error if
// non-nil) — the dashboard surface had zero request-level logging until this
// was added, which is exactly what let a dashboard-created task silently
// fail to even reach the tasks table with no trace anywhere in core's logs.
type accessLogInterceptor struct{}

func NewAccessLogInterceptor() connect.Interceptor {
	return accessLogInterceptor{}
}

func (accessLogInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		attrs := []any{"procedure", req.Spec().Procedure, "duration_ms", time.Since(start).Milliseconds()}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		slog.Info("dashboard rpc", attrs...)
		return resp, err
	}
}

func (accessLogInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (accessLogInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		attrs := []any{"procedure", conn.Spec().Procedure, "duration_ms", time.Since(start).Milliseconds()}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		slog.Info("dashboard rpc", attrs...)
		return err
	}
}
