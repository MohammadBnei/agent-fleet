// Package coreclient is the provisioner's gRPC client for core's
// CoreService — used only for ReportPodEvents (docs/adr/0020 point 3): the
// provisioner pushes pod-lifecycle events, core is the streaming server.
// This is the opposite direction from provisionerclient (core's client for
// ProvisionerService) — the provisioner is both a gRPC server (for core's
// commands) and, for this one call, a gRPC client.
package coreclient

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type Client struct {
	conn *grpc.ClientConn
	rpc  agentfleetv1.CoreServiceClient
}

// retryServiceConfig bounds-retries transient "produced zero addresses"
// blips (core is a single replica — any restart briefly empties the
// Service's DNS answer, surfaced to callers as codes.Unavailable) without
// hanging forever if core is actually down for longer than that.
const retryServiceConfig = `{
	"methodConfig": [{
		"name": [{"service": "agentfleet.v1.CoreService"}],
		"waitForReady": true,
		"retryPolicy": {
			"MaxAttempts": 5,
			"InitialBackoff": "0.5s",
			"MaxBackoff": "5s",
			"BackoffMultiplier": 2.0,
			"RetryableStatusCodes": ["UNAVAILABLE"]
		}
	}]
}`

func New(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("dial core: %w", err)
	}
	return &Client{conn: conn, rpc: agentfleetv1.NewCoreServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// ReportEvent pushes one pod-lifecycle event, opening a fresh single-event
// stream per call. A long-lived stream kept open across the provisioner's
// whole lifetime would be more "live," but pod events are inherently
// bursty and rare (one task's worth every few minutes, not a firehose) —
// one short-lived stream per event is simpler and avoids reconnect/backoff
// logic for a connection that would mostly sit idle anyway.
func (c *Client) ReportEvent(ctx context.Context, event *agentfleetv1.PodEvent) {
	stream, err := c.rpc.ReportPodEvents(ctx)
	if err != nil {
		slog.Warn("ReportEvent: open stream failed", "taskId", event.GetTaskId(), "error", err)
		return
	}
	if err := stream.Send(event); err != nil {
		slog.Warn("ReportEvent: send failed", "taskId", event.GetTaskId(), "error", err)
		return
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		slog.Warn("ReportEvent: close failed", "taskId", event.GetTaskId(), "error", err)
	}
}
