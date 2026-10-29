// Package errkeys is core's error keys (mountain plan/15 §4): a stable key, carried beside the code as an ErrorKey
// detail (proto/agentfleet/v1/errors.proto), so a client branches on it and never on a message. The registry is
// closed: TestErrKeys refuses a key that is not one of these constants, given to Connect or Status directly.
//
// Every error a Connect or gRPC handler returns carries a key: one built here carries its own, and the interceptors
// key whatever arrives without one as KeyInternal, keeping its code and words (a copy: the handler's error is never
// changed). Errors Connect or gRPC raise before a handler runs (an unreadable body, an unknown procedure) carry none,
// and neither do core's own HTTP gate's plain 401 and 403 (internal/auth), which answer before Connect does.
//
// A specific key is not coming to every site (docs/adr/0081 §2). A site gets one only when a client must act on the
// error or show text for it and the code alone does not say what happened; every other site stays KeyInternal with
// its specific code, and where it came from is answered by its trace id, not its key. A service declares its keys in
// its proto (§1): AgentService's are agent.proto's AgentErrorKey, each RPC listing its own, and the constants below
// are the Go registry of them (TestDeclared holds the two to each other). The detail and the fallback are ukubi-go's
// errs/connectkeys (§5), with agentfleet.v1.ErrorKey its one template; this package is core's wrapper of it: the
// registry, and the wiring connectkeys/wiring checks (TestWiring).
package errkeys

import (
	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/MohammadBnei/ukubi-go/errs"
	"github.com/MohammadBnei/ukubi-go/errs/connectkeys"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const (
	// KeyInternal is the fallback's key for an error built without one: its code says what it was. It is
	// connectkeys.KeyInternal, the Connect wire key (TestInternalIsConnectkeys).
	KeyInternal errs.Key = "server.internal"
	// KeyDefinitionChanged: the thread's agent definition changed since it opened (docs/adr/0066); start a new one.
	KeyDefinitionChanged errs.Key = "agent.definition_changed"
	// KeyThreadCap: the owner's open threads are at the cap; close one first.
	KeyThreadCap errs.Key = "agent.thread_cap"
	// KeyThreadUntrusted: the thread holds untrusted input (docs/adr/0067 §2); only an admin posts or answers in it.
	KeyThreadUntrusted errs.Key = "agent.thread_untrusted"
	// KeyDefinitionUnavailable: the definition does not exist or is not the caller's to use.
	KeyDefinitionUnavailable errs.Key = "agent.definition_unavailable"
)

// keys is the one detail template, agentfleet.v1.ErrorKey (docs/adr/0081 §5): the wire is what errkeys sent.
var keys = connectkeys.New(&agentfleetv1.ErrorKey{})

// Connect is Connect's error for code with err's words, keyed.
func Connect(code connect.Code, key errs.Key, err error) *connect.Error {
	return keys.Connect(code, key, err)
}

// Status is gRPC's error for code with msg, keyed.
func Status(code codes.Code, key errs.Key, msg string) error { return keys.Status(code, key, msg) }

// Of is the key an error carries, Connect's or gRPC's, or "" without one.
func Of(err error) errs.Key { return keys.Of(err) }

// WithInterceptors is a Connect handler's interceptors, ics, behind the fallback: outermost, it keys an error the
// handler or any of ics returns without one as KeyInternal, keeping its code and words, on a copy of it. Every handler
// core serves takes it as its first option, never connect.WithInterceptors (TestWiring).
func WithInterceptors(ics ...connect.Interceptor) connect.HandlerOption {
	return keys.WithInterceptors(ics...)
}

// UnaryServerInterceptor and StreamServerInterceptor key an error a gRPC handler returns without one as KeyInternal,
// keeping its code and message. Chain them first.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor { return keys.UnaryServerInterceptor() }

func StreamServerInterceptor() grpc.StreamServerInterceptor { return keys.StreamServerInterceptor() }
