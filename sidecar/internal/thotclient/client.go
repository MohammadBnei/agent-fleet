// Package thotclient is the sidecar's *second* outbound connection — the
// one deliberate exception to docs/adr/0020 point 5's "two local entry
// points, one upstream channel" shape, recorded in docs/adr/0035.
//
// Everything else the sidecar does still funnels through coreclient. This
// exists only so a worker can ask thot a question and get an answer
// synchronously, without the round trip being brokered by core (the whole
// point of the exception: real-time reachability).
//
// Persistence is NOT bypassed: the ask_thot handler writes the exchange
// back into the asking task's transcript via coreclient, so the audit
// trail stays intact and core remains the sole Postgres holder.
package thotclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type Client struct {
	conn   *grpc.ClientConn
	rpc    agentfleetv1.ThotServiceClient
	taskID string
	token  string
}

// Same retry shape coreclient uses, for the same reason: thot is a single
// replica, so any restart briefly empties its Service's DNS answer.
const retryServiceConfig = `{
	"methodConfig": [{
		"name": [{"service": "agentfleet.v1.ThotService"}],
		"waitForReady": true,
		"retryPolicy": {
			"MaxAttempts": 5,
			"InitialBackoff": "0.5s",
			"MaxBackoff": "5s",
			"BackoffMultiplier": 2.0,
			"RetryableStatusCodes": ["UNAVAILABLE", "CANCELLED"]
		}
	}]
}`

func New(addr, taskID, token string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("dial thot: %w", err)
	}
	return &Client{
		conn:   conn,
		rpc:    agentfleetv1.NewThotServiceClient(conn),
		taskID: taskID,
		token:  token,
	}, nil
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Ask puts a question to thot and blocks for the answer. taskID is
// supplied by the client (not the caller) for the same reason coreclient
// does it: one sidecar only ever serves one task.
func (c *Client) Ask(ctx context.Context, question string) (string, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	resp, err := c.rpc.AskThot(ctx, &agentfleetv1.AskThotRequest{
		AskingTaskId: c.taskID,
		Question:     question,
	})
	if err != nil {
		return "", fmt.Errorf("ask thot: %w", err)
	}
	return resp.GetAnswer(), nil
}
