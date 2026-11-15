package coreserver

import (
	"context"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/workerspans"
)

// SetSpanSink wires worker-span export (docs/adr/0081 §7). nil (no trace
// endpoint) makes ExportSpans accept nothing.
func (s *Server) SetSpanSink(sink *workerspans.Sink) { s.spans = sink }

// ExportSpans takes a worker's timings from its sidecar. The auth interceptor
// has already bound session_id to the caller's lease (and refuses the
// provisioner token), so the session named here is the caller's own; nothing
// else in the request is trusted for identity.
func (s *Server) ExportSpans(_ context.Context, req *agentfleetv1.ExportSpansRequest) (*agentfleetv1.ExportSpansResponse, error) {
	return &agentfleetv1.ExportSpansResponse{Accepted: int32(s.spans.Submit(req.GetSessionId(), req.GetSpans()))}, nil
}
