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

// repoSyncTimeout bounds SyncRepoCache, which cannot share the constant above:
// that call IS a clone when the cache is cold, and a first clone of a large
// repo blows two minutes on its own.
//
// Deliberately longer than sessionCallTimeout even though a CreateWorkerPod
// for the same repo contends with it on the provisioner's per-repo lock. The
// shorter this is, the likelier a clone is killed part-way, and a half-cloned
// cache directory is unrecoverable: it is neither a repository nor empty, so
// every later `git clone` into it fails with "destination path already exists
// and is not an empty directory". Waiting is cheap; a poisoned cache is not.
// git.EnsureRepoCachedForSession is what keeps the contending session from
// paying for that wait.
//
// ponytail: a fixed synchronous ceiling. A repo big enough to blow ten minutes
// wants a background job with a status column, not a bigger number here.
const repoSyncTimeout = 10 * time.Minute

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

// TearDownWorker is a named wrapper over TearDownSession so callers read as
// intent rather than as an enum argument.
//
// Its TearDownE2e sibling is gone: with no sandbox (docs/adr/0048 §6) the
// provisioner answers "nothing to tear down" for SESSION_KIND_E2E, so every
// call was a round trip to be told nothing happened.
func (c *Client) TearDownWorker(ctx context.Context, sessionID string) error {
	_, err := c.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_WORKER)
	return err
}

// SweepSession reclaims a session's disk: its working-tree PVC and its
// per-session SDK state.
//
// Deletes whole subtrees rather than named files. An earlier design passed
// the SDK's session id so the provisioner could remove `<sid>.jsonl`, which
// required the fleet to know the SDK's internal layout — and got it wrong,
// missing the sibling `<sid>/subagents/` directory that a live installation
// turned out to have. Removing a directory the fleet itself created needs no
// such knowledge and cannot drift when the SDK changes.
//
// repo is no longer a parameter: there is no branch to consider deleting, and
// the two things being removed are both keyed by session alone.
func (c *Client) SweepSession(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	if _, err := c.rpc.SweepSession(ctx, &agentfleetv1.SweepSessionRequest{SessionId: sessionID}); err != nil {
		return fmt.Errorf("SweepSession: %w", err)
	}
	return nil
}

// ExposeSession publishes a port from a session's pod and returns its URL.
func (c *Client) ExposeSession(ctx context.Context, sessionID string, port int32) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.ExposeSession(ctx, &agentfleetv1.ExposeSessionRequest{SessionId: sessionID, Port: port})
	if err != nil {
		return "", fmt.Errorf("ExposeSession: %w", err)
	}
	return resp.GetUrl(), nil
}

func (c *Client) UnexposeSession(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	if _, err := c.rpc.UnexposeSession(ctx, &agentfleetv1.UnexposeSessionRequest{SessionId: sessionID}); err != nil {
		return fmt.Errorf("UnexposeSession: %w", err)
	}
	return nil
}

// ProvisionService provisions or reuses a shared instance for (repo, kind).
// The caller resolves repo from the session row — see coreserver's
// RequestService for why that is not taken from the pod.
func (c *Client) ProvisionService(ctx context.Context, sessionID, repo, kind string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.ProvisionService(ctx, &agentfleetv1.ProvisionServiceRequest{SessionId: sessionID, Repo: repo, Kind: kind})
	if err != nil {
		return "", fmt.Errorf("ProvisionService: %w", err)
	}
	return resp.GetDsn(), nil
}

// SyncRepoCache refreshes one repo's clone cache without creating a pod —
// the dashboard's manual sync button (docs/adr/0028's repos table gained no
// column for this; the button reports its own outcome).
//
// Returns the response message rather than unpacking it: every field is a
// statistic headed straight for the operator's screen, and a second struct
// in between would only be a place for them to drift.
//
// repo and repoURL must come from the `repos` row, not from the request that
// triggered the sync — same rule as ProvisionService above.
func (c *Client) SyncRepoCache(ctx context.Context, repo, repoURL string) (*agentfleetv1.SyncRepoCacheResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, repoSyncTimeout)
	defer cancel()
	resp, err := c.rpc.SyncRepoCache(ctx, &agentfleetv1.SyncRepoCacheRequest{Repo: repo, RepoUrl: repoURL})
	if err != nil {
		return nil, fmt.Errorf("SyncRepoCache: %w", err)
	}
	return resp, nil
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
//
// image is `repos.image`, the column that replaced the four toolchain
// ingredients (docs/adr/0048 §6). Empty means the provisioner's own default.
// It rides here rather than being looked up provisioner-side for the same
// reason repoURL/baseBranch do: the provisioner holds no DB credentials
// (docs/adr/0020 point 1), so anything it needs arrives at pod-creation time.
//
// The result is a struct rather than three bare strings: pod name and the two
// image refs are all strings, and this repo has already shipped one silent
// same-typed transposition (agent-fleet/docs/verification-traps.md).
type WorkerPod struct {
	PodName string
	// The images the pod spec actually got, after the repo's `image` override
	// was resolved against the provisioner's defaults. Stored on the session
	// row so the dashboard can report the build a session is running.
	WorkerImage  string
	SidecarImage string
}

func (c *Client) CreateWorkerPod(ctx context.Context, sessionID, repo, repoURL, baseBranch, description, leaseID, resumeAgentSessionID string, resumeFromSeq int64, toolKeys []string, image string) (WorkerPod, error) {
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
		Image:           image,
	})
	if err != nil {
		return WorkerPod{}, fmt.Errorf("CreateWorkerPod: %w", err)
	}
	return WorkerPod{
		PodName:      resp.GetPodName(),
		WorkerImage:  resp.GetWorkerImage(),
		SidecarImage: resp.GetSidecarImage(),
	}, nil
}

// GetVersion reports the provisioner's build and the images it stamps onto new
// pods. Cheap by construction on the provisioner side (process-local
// constants, no Kubernetes call), which is why the dashboard's topology can
// ask on every 5s poll instead of caching it and going stale across a deploy.
func (c *Client) GetVersion(ctx context.Context) (version, workerImage, sidecarImage string, err error) {
	ctx, cancel := context.WithTimeout(ctx, sessionCallTimeout)
	defer cancel()
	resp, err := c.rpc.GetVersion(ctx, &agentfleetv1.GetVersionRequest{})
	if err != nil {
		return "", "", "", fmt.Errorf("GetVersion: %w", err)
	}
	return resp.GetVersion(), resp.GetWorkerImage(), resp.GetSidecarImage(), nil
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
