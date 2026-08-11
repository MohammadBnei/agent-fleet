// Package thotclient is core's outbound connection to thot — used only by
// the audit loop, to tell thot to run a scheduled check.
//
// Note the direction: core commands thot here, the same way it commands
// the provisioner (docs/adr/0020 point 2). thot's *findings* still come
// back through CoreService like every other component's, so this doesn't
// give thot a second persistence path.
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
	conn  *grpc.ClientConn
	rpc   agentfleetv1.ThotServiceClient
	token string
}

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

// New returns nil (not an error) when addr is empty — thot is optional,
// and a fleet deployed without it should simply never schedule audits
// rather than fail to start.
func New(addr, token string) (*Client, error) {
	if addr == "" {
		return nil, nil
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("dial thot: %w", err)
	}
	return &Client{conn: conn, rpc: agentfleetv1.NewThotServiceClient(conn), token: token}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) RunAudit(ctx context.Context, auditID, name, prompt string) (string, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	resp, err := c.rpc.RunAudit(ctx, &agentfleetv1.RunAuditRequest{
		AuditId: auditID, Name: name, Prompt: prompt,
	})
	if err != nil {
		return "", fmt.Errorf("run audit: %w", err)
	}
	return resp.GetStatus(), nil
}
