// Package provisionerclient is core's gRPC client for the provisioner's
// ProvisionerService — per docs/adr/0020's hub-and-spoke rule, core is the
// *only* caller of this service anywhere in the fleet (no worker pod or
// sidecar ever talks to the provisioner directly, including for e2e
// requests, which are proxied through core.proto's CoreService instead).
// Originally e2e-only (docs/adr/0013); grew CreateWorkerPod/TearDownSession
// when the provisioner also took over worker-pod lifecycle (docs/adr/0019).
package provisionerclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type Client struct {
	conn *grpc.ClientConn
	rpc  agentfleetv1.ProvisionerServiceClient
}

// sessionCallTimeout bounds CreateWorkerPod/TearDownSession specifically —
// the two calls whose server-side work (git clone/fetch/worktree-add, a
// k8s Job create/delete) can genuinely take a while under normal load, but
// must never take forever. Without this, a stuck downstream step hangs
// whichever caller invoked it indefinitely — for CreateWorkerPod's one
// caller, core's single-goroutine dispatch loop (dispatch.Loop.tick), that
// means the entire fleet's dispatch freezes on one wedged task, since
// nothing else ever proceeds past a tick() call that never returns
// (confirmed live: a task's CreateWorkerPod call hung with no error, no
// pod ever created, and every other pending task stuck behind it).
const sessionCallTimeout = 2 * time.Minute

// New dials addr (in-cluster ClusterIP, no TLS needed — Cilium's own
// network policy is the trust boundary here, same as every other
// in-cluster call in this fleet).
func New(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial provisioner: %w", err)
	}
	return &Client{conn: conn, rpc: agentfleetv1.NewProvisionerServiceClient(conn)}, nil
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

// GetSessionStatus reports the current e2e session status/preview URL for
// taskID (status is "" when no session exists). Used by the dashboard
// (docs/adr/0014) to decide whether a task's code-server link should show.
func (c *Client) GetSessionStatus(ctx context.Context, taskID string) (status, previewURL string, err error) {
	resp, err := c.rpc.GetE2ESessionStatus(ctx, &agentfleetv1.GetE2ESessionStatusRequest{
		TaskId: taskID,
	})
	if err != nil {
		return "", "", fmt.Errorf("GetE2ESessionStatus: %w", err)
	}
	return resp.GetStatus(), resp.GetPreviewUrl(), nil
}

// CreateE2eSession asks the provisioner to spin up an on-demand e2e preview
// pod for taskID (docs/adr/0012), proxied from CoreService.RequestE2eEnv —
// the sidecar/worker never call this directly (docs/adr/0020 hub-and-spoke).
func (c *Client) CreateE2eSession(ctx context.Context, taskID, repo string) (status, previewURL string, err error) {
	resp, err := c.rpc.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
		TaskId: taskID,
		Repo:   repo,
	})
	if err != nil {
		return "", "", fmt.Errorf("CreateE2ESession: %w", err)
	}
	return resp.GetStatus(), resp.GetPreviewUrl(), nil
}

// ListE2eTools/CallE2eTool proxy the e2e pod's runtime-discovered Playwright
// tool set (docs/adr/0020 hub-and-spoke — sidecar -> core -> provisioner ->
// e2e pod, this is the third hop).
func (c *Client) ListE2eTools(ctx context.Context, taskID string) ([]*agentfleetv1.E2EToolDescriptor, error) {
	resp, err := c.rpc.ListE2ETools(ctx, &agentfleetv1.ListE2EToolsRequest{TaskId: taskID})
	if err != nil {
		return nil, fmt.Errorf("ListE2ETools: %w", err)
	}
	return resp.GetTools(), nil
}

func (c *Client) CallE2eTool(ctx context.Context, taskID, toolName, argumentsJSON string) (resultJSON string, isError bool, err error) {
	resp, err := c.rpc.CallE2ETool(ctx, &agentfleetv1.CallE2EToolRequest{
		TaskId:        taskID,
		ToolName:      toolName,
		ArgumentsJson: argumentsJSON,
	})
	if err != nil {
		return "", false, fmt.Errorf("CallE2ETool: %w", err)
	}
	return resp.GetResultJson(), resp.GetIsError(), nil
}

// CreateWorkerPod asks the provisioner to clone/fetch/worktree-add (its own
// git-lifecycle ownership, docs/adr/0019 point 2) and spawn a two-container
// worker pod for taskID, returning once the pod is scheduled. Called only
// from core's own dispatch loop, immediately after it claims the task
// (docs/adr/0020 point 2 — core claims, then commands; the provisioner
// never claims tasks itself).
func (c *Client) CreateWorkerPod(ctx context.Context, taskID, repo, repoURL, baseBranch, description, guidance, leaseID, resumeSessionID string, resumeFromSeq int64) (podName string, err error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		TaskId:          taskID,
		Repo:            repo,
		RepoUrl:         repoURL,
		BaseBranch:      baseBranch,
		Description:     description,
		Guidance:        guidance,
		LeaseId:         leaseID,
		ResumeSessionId: resumeSessionID,
		ResumeFromSeq:   resumeFromSeq,
	})
	if err != nil {
		return "", fmt.Errorf("CreateWorkerPod: %w", err)
	}
	return resp.GetPodName(), nil
}

// TearDownSession commands the provisioner to tear down a worker or e2e
// session — core owns `tasks` and decides when a session should end
// (task reached done/failed/cancelled, or an explicit kill); the
// provisioner never polls/joins against task status itself
// (docs/adr/0020 point 1).
func (c *Client) TearDownSession(ctx context.Context, taskID string, kind agentfleetv1.SessionKind) (tornDown bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.TearDownSession(ctx, &agentfleetv1.TearDownSessionRequest{
		TaskId: taskID,
		Kind:   kind,
	})
	if err != nil {
		return false, fmt.Errorf("TearDownSession: %w", err)
	}
	return resp.GetTornDown(), nil
}

// ListWorktrees/DeleteWorktree back the dashboard's manual worktree
// cleanup view (reliability-findings.md #2) — core has no PVC access
// itself, so this is a pure passthrough to the provisioner's own git.Manager.
func (c *Client) ListWorktrees(ctx context.Context) ([]*agentfleetv1.WorktreeInfo, error) {
	resp, err := c.rpc.ListWorktrees(ctx, &agentfleetv1.ListWorktreesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListWorktrees: %w", err)
	}
	return resp.GetWorktrees(), nil
}

func (c *Client) DeleteWorktree(ctx context.Context, taskID, repo string, alsoDeleteBranch bool) (deleted bool, err error) {
	resp, err := c.rpc.DeleteWorktree(ctx, &agentfleetv1.DeleteWorktreeRequest{
		TaskId:           taskID,
		Repo:             repo,
		AlsoDeleteBranch: alsoDeleteBranch,
	})
	if err != nil {
		return false, fmt.Errorf("DeleteWorktree: %w", err)
	}
	return resp.GetDeleted(), nil
}
