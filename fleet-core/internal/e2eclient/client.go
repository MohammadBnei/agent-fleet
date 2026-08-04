// Package e2eclient is fleet-core's gRPC client for e2e-provisioner's
// E2eProvisionerService — the one internal gRPC caller in the fleet (see
// docs/adr/0013). Used by the Discord /e2e-kill handler so it asks
// e2e-provisioner directly instead of writing e2e_sessions itself.
package e2eclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type Client struct {
	conn *grpc.ClientConn
	rpc  agentfleetv1.E2EProvisionerServiceClient
}

// New dials addr (in-cluster ClusterIP, no TLS needed — Cilium's own
// network policy is the trust boundary here, same as every other
// in-cluster call in this fleet).
func New(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial e2e-provisioner: %w", err)
	}
	return &Client{conn: conn, rpc: agentfleetv1.NewE2EProvisionerServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// KillSession requests teardown of the active e2e session for taskID.
// Returns false if there was no active session to kill (mirrors today's
// bot/src/db.ts's requestE2eKill return semantics).
func (c *Client) KillSession(ctx context.Context, taskID, idempotencyKey string) (bool, error) {
	resp, err := c.rpc.KillE2ESession(ctx, &agentfleetv1.KillE2ESessionRequest{
		TaskId:         taskID,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return false, fmt.Errorf("KillE2ESession: %w", err)
	}
	return resp.GetKilled(), nil
}
