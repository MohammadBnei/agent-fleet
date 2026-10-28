// Package errkeys is core's error keys (mountain plan/15 §4): a stable key, carried beside the code as an ErrorKey
// detail (proto/agentfleet/v1/errors.proto), so a client branches on it and never on a message. The registry is
// closed: TestErrKeys refuses a key that is not one of these constants.
//
// Every error a Connect or gRPC handler returns carries a key: one built here carries its own, and the interceptors
// key whatever arrives without one as KeyInternal, keeping its code and words. Errors Connect or gRPC raise before a
// handler runs (an unreadable body, an unknown procedure) carry none. The specific keys arrive package by package;
// until then a site's key is KeyInternal, and its code still says what it was.
package errkeys

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Key is one of core's error keys.
type Key string

const (
	// KeyInternal is the interceptors' key for an error built without one: its code says what it was.
	KeyInternal Key = "server.internal"
	// KeyDefinitionChanged: the thread's agent definition changed since it opened (docs/adr/0066); start a new one.
	KeyDefinitionChanged Key = "agent.definition_changed"
)

func detail(key Key) *agentfleetv1.ErrorKey { return &agentfleetv1.ErrorKey{Key: string(key)} }

// Connect is Connect's error for code with err's words, keyed.
func Connect(code connect.Code, key Key, err error) *connect.Error {
	e := connect.NewError(code, err)
	addConnect(e, key)
	return e
}

func addConnect(e *connect.Error, key Key) {
	d, err := connect.NewErrorDetail(detail(key))
	if err != nil {
		panic(err) // a two-string message always marshals
	}
	e.AddDetail(d)
}

// Status is gRPC's error for code with msg, keyed.
func Status(code codes.Code, key Key, msg string) error {
	s, err := status.New(code, msg).WithDetails(detail(key))
	if err != nil {
		panic(err) // a two-string message always marshals; an OK code is a caller's bug
	}
	return s.Err()
}

// Of is the key an error carries, Connect's or gRPC's, or "" without one.
func Of(err error) Key {
	var ce *connect.Error
	if errors.As(err, &ce) {
		for _, d := range ce.Details() {
			if v, derr := d.Value(); derr == nil {
				if k, ok := v.(*agentfleetv1.ErrorKey); ok {
					return Key(k.GetKey())
				}
			}
		}
		return ""
	}
	if s, ok := status.FromError(err); ok {
		for _, d := range s.Details() {
			if k, ok := d.(*agentfleetv1.ErrorKey); ok {
				return Key(k.GetKey())
			}
		}
	}
	return ""
}

// ConnectInterceptor keys an error a Connect handler (or an inner interceptor) returns without one as KeyInternal,
// keeping its code and words. Put it outermost.
func ConnectInterceptor() connect.Interceptor { return keyAll{} }

type keyAll struct{}

func (keyAll) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		return res, withConnectKey(err)
	}
}

func (keyAll) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (keyAll) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return withConnectKey(next(ctx, conn))
	}
}

func withConnectKey(err error) error {
	if err == nil || Of(err) != "" {
		return err
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		addConnect(ce, KeyInternal)
		return ce
	}
	e := connect.NewError(connect.CodeOf(err), err) // a plain error: Connect's own code for it, as it would answer
	addConnect(e, KeyInternal)
	return e
}

// UnaryServerInterceptor and StreamServerInterceptor key an error a gRPC handler returns without one as KeyInternal,
// keeping its code and message. Chain them first.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		res, err := next(ctx, req)
		return res, withStatusKey(err)
	}
}

func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		return withStatusKey(next(srv, ss))
	}
}

func withStatusKey(err error) error {
	if err == nil || Of(err) != "" {
		return err
	}
	s := status.Convert(err) // a plain error is codes.Unknown with its words, as gRPC would answer
	k, kerr := s.WithDetails(detail(KeyInternal))
	if kerr != nil {
		return err // an OK status carries no detail; never reached for a non-nil error
	}
	return k.Err()
}
