package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
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
			return nil, errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("missing required header"))
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
			return errkeys.Connect(connect.CodePermissionDenied, errkeys.KeyInternal, errors.New("missing required header"))
		}
		return next(ctx, conn)
	}
}

// accessLogInterceptor logs one line per RPC (method, duration, error if
// non-nil) — the dashboard surface had zero request-level logging until this
// was added, which is exactly what let a dashboard-created task silently
// fail to even reach the tasks table with no trace anywhere in core's logs.
//
// It also answers every call with a Trace-Id response header (docs/adr/0081 §4), so the console can show the id
// beside an error and the line here is found by it. It sits inside otelconnect (the span exists) and outside CSRF,
// so a refused call carries both; the header is set before next, as connect-go sends a unary call's handler headers
// with its error too. A stream gets it in its headers only: no trailer, the console reads the header.
type accessLogInterceptor struct{}

func NewAccessLogInterceptor() connect.Interceptor {
	return accessLogInterceptor{}
}

func (accessLogInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		if info, ok := connect.CallInfoForHandlerContext(ctx); ok {
			setTraceID(ctx, info.ResponseHeader())
		}
		resp, err := next(ctx, req)
		logRPC(ctx, req.Spec().Procedure, start, err)
		return resp, err
	}
}

func (accessLogInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (accessLogInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		setTraceID(ctx, conn.ResponseHeader())
		err := next(ctx, conn)
		logRPC(ctx, conn.Spec().Procedure, start, err)
		return err
	}
}

const traceIDHeader = "Trace-Id"

func setTraceID(ctx context.Context, h http.Header) {
	if id := errkeys.TraceID(ctx); id != "" {
		h.Set(traceIDHeader, id)
	}
}

// logRPC is the one line: InfoContext, so the logger adds trace_id and span_id. code and key are what the wire carries
// (wireCode, errkeys.WireKey). source is where an errkeys.Connect error was built ("" for one built otherwise).
func logRPC(ctx context.Context, procedure string, start time.Time, err error) {
	attrs := []any{"procedure", procedure, "duration_ms", time.Since(start).Milliseconds()}
	code, key := "ok", ""
	if err != nil {
		code, key = wireCode(err).String(), string(errkeys.WireKey(err))
		attrs = append(attrs, "error", err.Error())
	}
	attrs = append(attrs, "code", code, "key", key, "source", errkeys.LocationOf(err))
	slog.InfoContext(ctx, "dashboard rpc", attrs...)
}

// wireCode is the code Connect sends for err: a Connect error's own, else a context error's, else Unknown (connect-go's
// wrapIfContextError, and the fallback's plainCode). connect.CodeOf says Unknown for a bare ctx.Err().
func wireCode(err error) connect.Code {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return ce.Code()
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return connect.CodeDeadlineExceeded
	}
	return connect.CodeUnknown
}
