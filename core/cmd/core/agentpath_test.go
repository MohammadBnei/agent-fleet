package main

import (
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/agentsvc"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// run.go gates members on the constant path, built before the handler. It
// must be the path the handler is mounted on, or members reach nothing (or
// the Gate's member route covers the wrong prefix).
func TestAgentServicePathMatchesHandler(t *testing.T) {
	path, _ := agentfleetv1connect.NewAgentServiceHandler(agentsvc.NewServer(nil, nil, nil, nil, nil))
	if want := "/" + agentfleetv1connect.AgentServiceName + "/"; path != want {
		t.Fatalf("handler path %q, gate prefix %q", path, want)
	}
}
