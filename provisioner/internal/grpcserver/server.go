// Package grpcserver implements ProvisionerService — renamed and grown
// from E2eProvisionerService (docs/adr/0019/0020). The provisioner holds no
// Postgres credentials at all: every method here treats Kubernetes itself
// as the durable source of truth for session/pod state (pod existence and
// phase), never a database row. core is the only caller (docs/adr/0020's
// hub-and-spoke rule) — no worker pod or sidecar ever reaches this service
// directly, including for e2e requests.
package grpcserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/mcpproxy"
)

// EventReporter is the narrow slice of coreclient.Client this server
// needs — a hand-written fake satisfies it in tests, no live gRPC
// connection to core required.
type EventReporter interface {
	ReportEvent(ctx context.Context, event *agentfleetv1.PodEvent)
}

type Server struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	k8sc    *k8s.Client
	git     *git.Manager
	proxy   *mcpproxy.Proxy
	core    EventReporter
	e2eHost string
	// FleetShared* configure git.Manager.SyncFleetShared's source (docs/adr/
	// 0031) — empty FleetSharedRepoURL disables the sync entirely (used by
	// tests that don't care about fleet-shared content).
	fleetSharedRepoURL string
	fleetSharedBranch  string
	claudeHomeDir      string
}

func New(k8sc *k8s.Client, gitMgr *git.Manager, proxy *mcpproxy.Proxy, core EventReporter, e2eHost string, fleetSharedRepoURL, fleetSharedBranch, claudeHomeDir string) *Server {
	return &Server{
		k8sc: k8sc, git: gitMgr, proxy: proxy, core: core, e2eHost: e2eHost,
		fleetSharedRepoURL: fleetSharedRepoURL, fleetSharedBranch: fleetSharedBranch, claudeHomeDir: claudeHomeDir,
	}
}

// --- e2e sessions (unchanged behavior, k8s-backed instead of Postgres-backed) ---

func (s *Server) KillE2ESession(ctx context.Context, req *agentfleetv1.KillE2ESessionRequest) (*agentfleetv1.KillE2ESessionResponse, error) {
	_, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(req.GetTaskId()))
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.KillE2ESessionResponse{Killed: false}, nil
	}
	if err := s.k8sc.DeleteAll(ctx, req.GetTaskId()); err != nil {
		return nil, err
	}
	s.proxy.DropClient(req.GetTaskId())
	slog.Info("grpcserver KillE2ESession", "taskId", req.GetTaskId())
	return &agentfleetv1.KillE2ESessionResponse{Killed: true}, nil
}

func (s *Server) GetE2ESessionStatus(ctx context.Context, req *agentfleetv1.GetE2ESessionStatusRequest) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	phase, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(req.GetTaskId()))
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.GetE2ESessionStatusResponse{}, nil
	}
	return &agentfleetv1.GetE2ESessionStatusResponse{
		Status:     e2eStatusFromPhase(phase),
		PreviewUrl: k8s.PreviewURLFor(s.e2eHost, req.GetTaskId()),
	}, nil
}

// CreateE2ESession is idempotent — "safe to call again if one is already
// running" (today's mcpserver docstring, preserved): a GET before create,
// not a create-and-handle-AlreadyExists, since it also needs to report the
// existing session's own status/URL, not just "yes it exists."
func (s *Server) CreateE2ESession(ctx context.Context, req *agentfleetv1.CreateE2ESessionRequest) (*agentfleetv1.CreateE2ESessionResponse, error) {
	phase, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(req.GetTaskId()))
	if err != nil {
		return nil, err
	}
	if exists {
		return &agentfleetv1.CreateE2ESessionResponse{
			Status:     e2eStatusFromPhase(phase),
			PreviewUrl: k8s.PreviewURLFor(s.e2eHost, req.GetTaskId()),
		}, nil
	}

	serviceRefs, extraEnv, err := s.resolveServiceIngredients(ctx, req.GetRepo(), req.GetTaskId(), req.GetServiceIngredients())
	if err != nil {
		return nil, fmt.Errorf("resolve service ingredients: %w", err)
	}
	taskRef := k8s.TaskRef{
		ID: req.GetTaskId(), Repo: req.GetRepo(), StartCmd: req.GetStartCmd(),
		ToolKeys: req.GetToolKeys(), ServiceIngredients: serviceRefs, ExtraEnv: extraEnv,
	}
	if err := s.k8sc.CreatePod(ctx, taskRef); err != nil {
		return nil, fmt.Errorf("create e2e pod: %w", err)
	}
	if err := s.k8sc.CreateService(ctx, req.GetTaskId()); err != nil {
		return nil, fmt.Errorf("create e2e service: %w", err)
	}
	if err := s.k8sc.CreateMiddleware(ctx, req.GetTaskId()); err != nil {
		return nil, fmt.Errorf("create e2e middleware: %w", err)
	}
	if err := s.k8sc.CreateIngressRoute(ctx, s.e2eHost, req.GetTaskId()); err != nil {
		return nil, fmt.Errorf("create e2e ingressroute: %w", err)
	}
	slog.Info("grpcserver CreateE2ESession", "taskId", req.GetTaskId(), "repo", req.GetRepo())
	return &agentfleetv1.CreateE2ESessionResponse{
		Status:     "running",
		PreviewUrl: k8s.PreviewURLFor(s.e2eHost, req.GetTaskId()),
	}, nil
}

// scopeModeString converts the proto ScopeMode enum into the plain string
// package k8s works with (decoupling k8s from the proto module, same
// reasoning as TaskRef not being a proto type).
func scopeModeString(m agentfleetv1.ScopeMode) string {
	switch m {
	case agentfleetv1.ScopeMode_SCOPE_MODE_POD_SCOPED:
		return k8s.ScopeModePodScoped
	case agentfleetv1.ScopeMode_SCOPE_MODE_TASK_SCOPED:
		return k8s.ScopeModeTaskScoped
	case agentfleetv1.ScopeMode_SCOPE_MODE_REPO_SCOPED:
		return k8s.ScopeModeRepoScoped
	default:
		return ""
	}
}

// resolveServiceIngredients converts the proto ServiceIngredient list into
// the plain k8s.ServiceIngredientRef shape CreatePod/CreateWorkerPod
// accept, and mints credentials for every non-pod-scoped entry
// synchronously, before the caller creates any pod — a minting failure
// here means the RPC itself errors and the consuming pod is never created
// (docs/adr/0034's fail-loud guarantee, stronger than a StartupProbe).
func (s *Server) resolveServiceIngredients(ctx context.Context, repo, taskID string, in []*agentfleetv1.ServiceIngredient) ([]k8s.ServiceIngredientRef, []corev1.EnvVar, error) {
	refs := make([]k8s.ServiceIngredientRef, 0, len(in))
	var extraEnv []corev1.EnvVar
	for _, si := range in {
		mode := scopeModeString(si.GetScopeMode())
		if mode == "" {
			return nil, nil, fmt.Errorf("service ingredient %q: unspecified scope mode", si.GetKey())
		}
		refs = append(refs, k8s.ServiceIngredientRef{Key: si.GetKey(), ScopeMode: mode})
		if mode == k8s.ScopeModePodScoped {
			continue // materialized as a same-pod sidecar by buildIngredients instead
		}
		url, err := s.k8sc.MintServiceCredentials(ctx, repo, taskID, si.GetKey(), mode)
		if err != nil {
			return nil, nil, fmt.Errorf("mint credentials for %s (%s): %w", si.GetKey(), mode, err)
		}
		extraEnv = append(extraEnv, corev1.EnvVar{
			Name:  fmt.Sprintf("SERVICE_%s_URL", strings.ToUpper(si.GetKey())),
			Value: url,
		})
	}
	return refs, extraEnv, nil
}

func e2eStatusFromPhase(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodPending:
		return "requested"
	case corev1.PodRunning:
		return "running"
	case corev1.PodFailed:
		return "failed"
	default:
		return "running"
	}
}

// --- Playwright tool passthrough (docs/adr/0020's third hop) ---

func (s *Server) ListE2ETools(ctx context.Context, req *agentfleetv1.ListE2EToolsRequest) (*agentfleetv1.ListE2EToolsResponse, error) {
	tools := s.proxy.ProxiedTools(ctx, req.GetTaskId())
	out := make([]*agentfleetv1.E2EToolDescriptor, len(tools))
	for i, t := range tools {
		schemaJSON, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("marshal input schema for tool %s: %w", t.Name, err)
		}
		out[i] = &agentfleetv1.E2EToolDescriptor{
			Name:            t.Name,
			Description:     t.Description,
			InputSchemaJson: string(schemaJSON),
		}
	}
	return &agentfleetv1.ListE2EToolsResponse{Tools: out}, nil
}

func (s *Server) CallE2ETool(ctx context.Context, req *agentfleetv1.CallE2EToolRequest) (*agentfleetv1.CallE2EToolResponse, error) {
	var args map[string]any
	if req.GetArgumentsJson() != "" {
		if err := json.Unmarshal([]byte(req.GetArgumentsJson()), &args); err != nil {
			return nil, fmt.Errorf("unmarshal tool arguments: %w", err)
		}
	}
	result, err := s.proxy.CallTool(ctx, req.GetTaskId(), req.GetToolName(), args)
	if err != nil {
		return nil, err
	}
	slog.Debug("grpcserver CallE2ETool", "taskId", req.GetTaskId(), "tool", req.GetToolName())
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return &agentfleetv1.CallE2EToolResponse{ResultJson: string(resultJSON), IsError: result.IsError}, nil
}

// --- worker pods (new, docs/adr/0019/0020) ---

func (s *Server) CreateWorkerPod(ctx context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	// Reported immediately, before any git/k8s work starts — otherwise a
	// claimed task with a hung or merely slow provisioning step is
	// indistinguishable from one the provisioner never received at all
	// (confirmed live: a task sat claimed for 20+ minutes with zero pod
	// events either way). PROVISIONING's `message` carries the current
	// sub-step rather than a new enum value per step, since these are
	// provisioner-internal detail, not states any other component branches
	// on — see core.proto's own comment on the enum value.
	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "cloning repo")
	if err := s.git.EnsureRepoCloned(ctx, req.GetRepo(), req.GetRepoUrl()); err != nil {
		s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "clone/fetch failed: "+err.Error())
		return nil, fmt.Errorf("ensure repo cloned: %w", err)
	}
	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "adding worktree")
	worktreePath, _, err := s.git.CreateWorktree(ctx, req.GetRepo(), req.GetTaskId(), req.GetBaseBranch())
	if err != nil {
		s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "worktree add failed: "+err.Error())
		return nil, fmt.Errorf("create worktree: %w", err)
	}
	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CREATED, "", "")

	// Best-effort, unlike the clone/worktree steps above: stale or
	// momentarily missing fleet-shared skills/context is a degraded worker
	// session, not a broken one, so a sync failure here logs and continues
	// rather than failing the whole dispatch (docs/adr/0032).
	if s.fleetSharedRepoURL != "" {
		s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "syncing fleet-shared skills")
		if err := s.git.SyncFleetShared(ctx, s.fleetSharedRepoURL, s.fleetSharedBranch, s.claudeHomeDir); err != nil {
			slog.Warn("grpcserver: fleet-shared sync failed, continuing with a possibly-stale copy", "taskId", req.GetTaskId(), "error", err)
		}
	}

	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "minting service credentials")
	serviceRefs, extraEnv, err := s.resolveServiceIngredients(ctx, req.GetRepo(), req.GetTaskId(), req.GetServiceIngredients())
	if err != nil {
		s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "ingredient resolution failed: "+err.Error())
		return nil, fmt.Errorf("resolve service ingredients: %w", err)
	}

	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "creating pod")
	if err := s.k8sc.CreateWorkerPod(ctx, req.GetTaskId(), req.GetRepo(), req.GetLeaseId(), worktreePath, req.GetResumeSessionId(), req.GetResumeFromSeq(), req.GetToolKeys(), serviceRefs, extraEnv); err != nil {
		s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "pod create failed: "+err.Error())
		return nil, fmt.Errorf("create worker pod: %w", err)
	}

	podName := k8s.WorkerResourceName(req.GetTaskId())
	s.reportEvent(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_SCHEDULED, podName, "")
	slog.Info("grpcserver CreateWorkerPod", "taskId", req.GetTaskId(), "repo", req.GetRepo(), "podName", podName)
	return &agentfleetv1.CreateWorkerPodResponse{PodName: podName}, nil
}

// TearDownSession is opportunistic by design (docs/adr/0020's Consequences
// — core calls this unconditionally on every terminal task status, for
// both kinds, and "nothing to tear down" is a correct, expected no-op, not
// an error).
func (s *Server) TearDownSession(ctx context.Context, req *agentfleetv1.TearDownSessionRequest) (*agentfleetv1.TearDownSessionResponse, error) {
	switch req.GetKind() {
	case agentfleetv1.SessionKind_SESSION_KIND_WORKER:
		return s.tearDownWorker(ctx, req.GetTaskId())
	case agentfleetv1.SessionKind_SESSION_KIND_E2E:
		return s.tearDownE2e(ctx, req.GetTaskId())
	default:
		return nil, fmt.Errorf("TearDownSession: unspecified session kind")
	}
}

// tearDownWorker no longer touches git state at all (reliability-findings.md
// #2) — for any status including "done", not just failure paths. The old
// unconditional branch -D here destroyed the only reference to a
// never-pushed branch's commits whenever a terminal status was reached via
// a git push failure. Worktree/branch cleanup now happens two other ways:
// the periodic sweep (provisioner/internal/sweep, [gone]-tracked branches)
// and manual dashboard deletion (ListWorktrees/DeleteWorktree, below).
func (s *Server) tearDownWorker(ctx context.Context, taskID string) (*agentfleetv1.TearDownSessionResponse, error) {
	_, exists, err := s.k8sc.GetWorkerJobRepo(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.TearDownSessionResponse{TornDown: false}, nil
	}
	if err := s.k8sc.DeleteWorkerJob(ctx, taskID); err != nil {
		return nil, err
	}
	s.reportEvent(ctx, taskID, agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_TERMINATED, "", "")
	slog.Info("grpcserver tearDownWorker", "taskId", taskID)
	return &agentfleetv1.TearDownSessionResponse{TornDown: true}, nil
}

func (s *Server) tearDownE2e(ctx context.Context, taskID string) (*agentfleetv1.TearDownSessionResponse, error) {
	_, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(taskID))
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.TearDownSessionResponse{TornDown: false}, nil
	}
	if err := s.k8sc.DeleteAll(ctx, taskID); err != nil {
		return nil, err
	}
	s.proxy.DropClient(taskID)
	slog.Info("grpcserver tearDownE2e", "taskId", taskID)
	return &agentfleetv1.TearDownSessionResponse{TornDown: true}, nil
}

// --- worktree lifecycle (reliability-findings.md #2) ---

func (s *Server) ListWorktrees(ctx context.Context, _ *agentfleetv1.ListWorktreesRequest) (*agentfleetv1.ListWorktreesResponse, error) {
	repos, err := s.git.ListClonedRepos()
	if err != nil {
		return nil, fmt.Errorf("list cloned repos: %w", err)
	}
	var out []*agentfleetv1.WorktreeInfo
	for _, repo := range repos {
		infos, err := s.git.ListWorktrees(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("list worktrees for %s: %w", repo, err)
		}
		for _, info := range infos {
			out = append(out, &agentfleetv1.WorktreeInfo{
				TaskId:        info.TaskID,
				Repo:          info.Repo,
				Branch:        info.Branch,
				UpstreamTrack: info.UpstreamTrack,
				MtimeUnix:     info.MtimeUnix,
			})
		}
	}
	return &agentfleetv1.ListWorktreesResponse{Worktrees: out}, nil
}

func (s *Server) DeleteWorktree(ctx context.Context, req *agentfleetv1.DeleteWorktreeRequest) (*agentfleetv1.DeleteWorktreeResponse, error) {
	if err := s.git.DeleteWorktree(ctx, req.GetRepo(), req.GetTaskId(), req.GetAlsoDeleteBranch()); err != nil {
		return nil, err
	}
	return &agentfleetv1.DeleteWorktreeResponse{Deleted: true}, nil
}

func (s *Server) reportEvent(ctx context.Context, taskID string, kind agentfleetv1.SessionKind, phase agentfleetv1.PodPhase, podName, message string) {
	s.core.ReportEvent(ctx, &agentfleetv1.PodEvent{
		TaskId:  taskID,
		Kind:    kind,
		Phase:   phase,
		PodName: podName,
		Message: message,
	})
}
