package grpcserver

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// A warm's steps must show up as warm.* spans nested under the handler's span,
// taking the tracer from the span in ctx (no global provider is set).
func TestCreateWorkerPod_EmitsWarmChildSpans(t *testing.T) {
	s, _, _ := newTestServer(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, handler := tp.Tracer("test").Start(context.Background(), "handler")

	if _, err := s.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId: "task-1", Repo: "dream-analyst", RepoUrl: newTestOriginRepo(t), BaseBranch: "main",
	}); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	handler.End()

	root := handler.SpanContext().SpanID()
	names := map[string]bool{}
	for _, sp := range rec.Ended() {
		names[sp.Name()] = true
		if sp.Name() == "handler" {
			continue
		}
		if sp.SpanContext().TraceID() != handler.SpanContext().TraceID() {
			t.Errorf("%s is in another trace", sp.Name())
		}
		// Direct children of the handler are the top-level steps.
		if sp.Name() == "warm.ensure_repo_fetched" && sp.Parent().SpanID() != root {
			t.Errorf("%s is not a child of the handler span", sp.Name())
		}
	}
	for _, want := range []string{
		"warm.report_event", "warm.ensure_repo_fetched", "warm.repo_lock_wait", "warm.git.clone",
		"warm.ensure_claude_home", "warm.replace_terminal_worker_job", "warm.write_genome",
		"warm.vault_lock_wait", "warm.mint_repo_token", "warm.github_owner_token", "warm.vault_reconcile_owner",
		"warm.vault_ca", "warm.ensure_vault_ca", "warm.vault_mint",
		"warm.k8s_create_worker_pod", "warm.ensure_session_pvc", "warm.job_create",
	} {
		if !names[want] {
			t.Errorf("missing span %s; got %v", want, names)
		}
	}
}
