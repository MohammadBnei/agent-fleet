package coreserver

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0084 §4: the thread a pod's turn runs in rides on every call it makes, and reaches the handler's
// appends through the interceptor. Only a session's own pod names one: the provisioner does not.
func TestInterceptor_PodsThreadReachesTheHandler(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s", lease: "l"}, "tok")
	info := &grpc.UnaryServerInfo{FullMethod: "/agentfleet.v1.CoreService/SendMessage"}
	req := &agentfleetv1.SendMessageRequest{SessionId: "s", From: "agent", Text: "x"}
	var got string
	handler := func(ctx context.Context, _ any) (any, error) {
		got = transcript.ThreadFrom(ctx)
		return nil, nil
	}
	if _, err := a.UnaryInterceptor(ctxWith(mdSessionID, "s", mdLeaseID, "l", mdThread, "topic-a"), req, info, handler); err != nil || got != "topic-a" {
		t.Fatalf("a pod's thread = %q, %v", got, err)
	}
	if _, err := a.UnaryInterceptor(ctxWith(mdSessionID, "s", mdLeaseID, "l"), req, info, handler); err != nil || got != transcript.MainThread {
		t.Fatalf("no thread named = %q, %v", got, err)
	}
	if _, err := a.UnaryInterceptor(ctxWith(mdProvisionerToken, "tok", mdThread, "topic-a"), req, info, handler); err != nil || got != transcript.MainThread {
		t.Fatalf("the provisioner named a thread: %q, %v", got, err)
	}
}
