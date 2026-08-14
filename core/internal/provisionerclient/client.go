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

// e2eCreateTimeout bounds CreateE2eSession, which had no deadline at all
// until docs/adr/0044 — the same defect sessionCallTimeout was introduced to
// fix, just on the path nobody had audited. Its blast radius is different:
// this call is driven by an agent's MCP tool call, so a wedged provisioner
// hangs the agent's turn rather than the dispatch loop.
//
// It cannot simply reuse sessionCallTimeout: the provisioner-side work here
// legitimately includes a 2-minute WaitForPodGone on a replaced pod plus up
// to 60s per EnsureSharedInstance and the credential-mint retries behind it.
// Must stay strictly greater than the sum of those bounded waits.
const e2eCreateTimeout = 5 * time.Minute

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
		SessionId:            taskID,
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
		SessionId: taskID,
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
// Returns the response whole, for the same reason GetSessionStatus does: it
// now carries the ServiceEndpoint roster (docs/adr/0045), which core forwards
// untouched. Picking two fields off it here is what would make the roster
// core's business.
func (c *Client) CreateE2eSession(ctx context.Context, sessionID, repo, startCmd string, toolKeys []string) (*agentfleetv1.CreateE2ESessionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, e2eCreateTimeout)
	defer cancel()
	resp, err := c.rpc.CreateE2ESession(ctx, &agentfleetv1.CreateE2ESessionRequest{
		SessionId: sessionID,
		Repo:      repo,
		StartCmd:  startCmd,
		ToolKeys:  toolKeys,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateE2ESession: %w", err)
	}
	return resp, nil
}

// ListWorkerPods returns sessionID -> Kubernetes Job phase for every live
// worker Job. Backs core's reconcile loop — the safety net that replaced the
// heartbeat (docs/adr/0048).
func (c *Client) ListWorkerPods(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.ListWorkerPods(ctx, &agentfleetv1.ListWorkerPodsRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListWorkerPods: %w", err)
	}
	out := make(map[string]string, len(resp.GetPods()))
	for _, p := range resp.GetPods() {
		out[p.GetSessionId()] = p.GetPhase()
	}
	return out, nil
}

// TearDownWorker/TearDownE2e are named wrappers over TearDownSession so the
// reconcile loop reads as intent rather than as an enum argument.
func (c *Client) TearDownWorker(ctx context.Context, sessionID string) error {
	_, err := c.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_WORKER)
	return err
}

func (c *Client) TearDownE2e(ctx context.Context, sessionID string) error {
	_, err := c.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_E2E)
	return err
}

// SweepSession reclaims a session's disk: its working directory and its
// per-session SDK state.
//
// Deletes whole subtrees rather than named files. An earlier design passed
// the SDK's session id so the provisioner could remove `<sid>.jsonl`, which
// required the fleet to know the SDK's internal layout — and got it wrong,
// missing the sibling `<sid>/subagents/` directory that a live installation
// turned out to have. Removing a directory the fleet itself created needs no
// such knowledge and cannot drift when the SDK changes.
func (c *Client) SweepSession(ctx context.Context, sessionID, repo string) error {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	// alsoDeleteBranch is false: the fleet no longer creates branches, so it
	// has no business deleting them. A branch the agent pushed is GitHub's to
	// keep or drop, and one it never pushed is uncommitted work.
	_, err := c.DeleteWorktree(ctx, sessionID, repo, false)
	if err != nil {
		return fmt.Errorf("SweepSession: %w", err)
	}
	return nil
}

// ToolKeysFor is the whole of what survives docs/adr/0034's ingredient
// resolution. The recipe used to answer three questions — how to start the
// app, what toolchain it needs, what services it needs — and docs/adr/0048
// found that the agent can read the first two off the repo it is sitting in.
//
// cluster-access is the one that could not move, because it is not a
// toolchain: it stages a kubectl shim that RPCs to thot-executor, and whether
// a repo's sessions may do that is a human's decision about privilege, not a
// fact about the codebase (docs/adr/0037).
//
// The service ingredients have no producer left at all — they become the
// request_service MCP tool, which stays fleet-side only because provisioning
// a shared Postgres needs cluster RBAC the agent does not have.
func ToolKeysFor(clusterAccess bool) []string {
	if !clusterAccess {
		return nil
	}
	return []string{"cluster-access"}
}

// CreateWorkerPod asks the provisioner to clone/fetch/worktree-add (its own
// git-lifecycle ownership, docs/adr/0019 point 2) and spawn a two-container
// worker pod for sessionID, returning once the pod is scheduled.
//
// Called when a session's first message arrives with no live pod — core
// decides, the provisioner executes (docs/adr/0020 point 2). There is no
// dispatch loop any more: a message is the only thing that provisions, which
// is what keeps a machine-initiated proposal from ever producing a pod.
//
// toolKeys carries only cluster-access now (see ToolKeysFor); `guidance` is
// gone with the column, since operator snippets reach the model as message
// text a human sent rather than as a wrapper the agent cannot see.
func (c *Client) CreateWorkerPod(ctx context.Context, sessionID, repo, repoURL, baseBranch, description, leaseID, resumeAgentSessionID string, resumeFromSeq int64, toolKeys []string) (podName string, err error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.CreateWorkerPod(ctx, &agentfleetv1.CreateWorkerPodRequest{
		SessionId:       sessionID,
		Repo:            repo,
		RepoUrl:         repoURL,
		BaseBranch:      baseBranch,
		Description:     description,
		LeaseId:         leaseID,
		ResumeSessionId: resumeAgentSessionID,
		ResumeFromSeq:   resumeFromSeq,
		ToolKeys:        toolKeys,
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
		SessionId: taskID,
		Kind:      kind,
	})
	if err != nil {
		return false, fmt.Errorf("TearDownSession: %w", err)
	}
	return resp.GetTornDown(), nil
}

// ListWorktrees/DeleteWorktree back the dashboard's manual worktree
// cleanup view (reliability-findings.md #2) — core has no PVC access
// itself, so this is a pure passthrough to the provisioner's own git.Manager.
// Returns the whole response, not just the slice: it now also carries the
// shared PVC's total/free bytes, which belong to the filesystem rather than to
// any one worktree.
func (c *Client) ListWorktrees(ctx context.Context) (*agentfleetv1.ListWorktreesResponse, error) {
	resp, err := c.rpc.ListWorktrees(ctx, &agentfleetv1.ListWorktreesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListWorktrees: %w", err)
	}
	return resp, nil
}

func (c *Client) DeleteWorktree(ctx context.Context, taskID, repo string, alsoDeleteBranch bool) (deleted bool, err error) {
	resp, err := c.rpc.DeleteWorktree(ctx, &agentfleetv1.DeleteWorktreeRequest{
		SessionId:        taskID,
		Repo:             repo,
		AlsoDeleteBranch: alsoDeleteBranch,
	})
	if err != nil {
		return false, fmt.Errorf("DeleteWorktree: %w", err)
	}
	return resp.GetDeleted(), nil
}
