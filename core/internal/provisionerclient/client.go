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

	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
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
// bot/src/db.ts's requestE2eKill return semantics). alsoTeardownServices
// additionally deletes repo's shared postgres/redis instances — opt-in,
// human-confirmed only (the dashboard's "kill e2e" checkbox); repo is
// ignored when alsoTeardownServices is false.
func (c *Client) KillSession(ctx context.Context, taskID, idempotencyKey, repo string, alsoTeardownServices bool) (killed bool, servicesTornDown []string, err error) {
	resp, err := c.rpc.KillE2ESession(ctx, &agentfleetv1.KillE2ESessionRequest{
		TaskId:               taskID,
		IdempotencyKey:       idempotencyKey,
		Repo:                 repo,
		AlsoTeardownServices: alsoTeardownServices,
	})
	if err != nil {
		return false, nil, fmt.Errorf("KillE2ESession: %w", err)
	}
	return resp.GetKilled(), resp.GetServicesTornDown(), nil
}

// GetSessionStatus reports the current e2e session state for taskID (status
// is "" when no session exists). Used by the dashboard (docs/adr/0014) to
// decide whether a task's code-server link should show, and to render the
// e2e card's live pod state. Returns the response whole rather than picking
// fields off it — every field here is live pod truth only the provisioner
// can read, and core is a pass-through for all of it.
func (c *Client) GetSessionStatus(ctx context.Context, taskID string) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	resp, err := c.rpc.GetE2ESessionStatus(ctx, &agentfleetv1.GetE2ESessionStatusRequest{
		TaskId: taskID,
	})
	if err != nil {
		return nil, fmt.Errorf("GetE2ESessionStatus: %w", err)
	}
	return resp, nil
}

// CreateE2eSession asks the provisioner to spin up an on-demand e2e preview
// pod for taskID (docs/adr/0012), proxied from CoreService.RequestE2eEnv —
// the sidecar/worker never call this directly (docs/adr/0020 hub-and-spoke).
// toolKeys/serviceIngredients are the repo's resolved "e2e" profile
// (docs/adr/0034, empty when it has none — preserves the pre-recipe pod
// shape); the provisioner mints any non-pod-scoped service credentials
// itself, core never sees them.
func (c *Client) CreateE2eSession(ctx context.Context, taskID, repo, startCmd string, toolKeys []string, serviceIngredients []repoprofiles.ServiceIngredient) (status, previewURL string, err error) {
	protoIngredients, err := toProtoServiceIngredients(serviceIngredients)
	if err != nil {
		return "", "", fmt.Errorf("CreateE2ESession: %w", err)
	}
	resp, err := c.rpc.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
		TaskId:             taskID,
		Repo:               repo,
		StartCmd:           startCmd,
		ToolKeys:           toolKeys,
		ServiceIngredients: protoIngredients,
	})
	if err != nil {
		return "", "", fmt.Errorf("CreateE2ESession: %w", err)
	}
	return resp.GetStatus(), resp.GetPreviewUrl(), nil
}

// toProtoServiceIngredients/toProtoScopeMode convert repoprofiles' plain
// string scope-mode ("pod-scoped"/"task-scoped"/"repo-scoped", the same
// values stored in repo_profile_services.scope_mode) into the proto
// ScopeMode enum the provisioner's wire format expects.
func toProtoServiceIngredients(in []repoprofiles.ServiceIngredient) ([]*agentfleetv1.ServiceIngredient, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]*agentfleetv1.ServiceIngredient, 0, len(in))
	for _, si := range in {
		mode, err := toProtoScopeMode(si.ScopeMode)
		if err != nil {
			return nil, err
		}
		out = append(out, &agentfleetv1.ServiceIngredient{Key: si.Key, ScopeMode: mode})
	}
	return out, nil
}

func toProtoScopeMode(s string) (agentfleetv1.ScopeMode, error) {
	switch s {
	case "pod-scoped":
		return agentfleetv1.ScopeMode_SCOPE_MODE_POD_SCOPED, nil
	case "task-scoped":
		return agentfleetv1.ScopeMode_SCOPE_MODE_TASK_SCOPED, nil
	case "repo-scoped":
		return agentfleetv1.ScopeMode_SCOPE_MODE_REPO_SCOPED, nil
	default:
		return agentfleetv1.ScopeMode_SCOPE_MODE_UNSPECIFIED, fmt.Errorf("unknown scope mode %q", s)
	}
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
// never claims tasks itself). toolKeys/serviceIngredients are the repo's
// resolved "worker" profile (docs/adr/0034, empty when it has none —
// preserves the pre-recipe pod shape).
func (c *Client) CreateWorkerPod(ctx context.Context, taskID, repo, repoURL, baseBranch, description, guidance, leaseID, resumeSessionID string, resumeFromSeq int64, toolKeys []string, serviceIngredients []repoprofiles.ServiceIngredient) (podName string, err error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	protoIngredients, err := toProtoServiceIngredients(serviceIngredients)
	if err != nil {
		return "", fmt.Errorf("CreateWorkerPod: %w", err)
	}
	resp, err := c.rpc.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		TaskId:             taskID,
		Repo:               repo,
		RepoUrl:            repoURL,
		BaseBranch:         baseBranch,
		Description:        description,
		Guidance:           guidance,
		LeaseId:            leaseID,
		ResumeSessionId:    resumeSessionID,
		ResumeFromSeq:      resumeFromSeq,
		ToolKeys:           toolKeys,
		ServiceIngredients: protoIngredients,
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
