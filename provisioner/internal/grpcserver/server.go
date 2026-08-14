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
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
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
	core    EventReporter
	e2eHost string
	// FleetShared* configure git.Manager.SyncFleetShared's source (docs/adr/
	// 0031) — empty FleetSharedRepoURL disables the sync entirely (used by
	// tests that don't care about fleet-shared content).
	fleetSharedRepoURL string
	fleetSharedBranch  string
	claudeHomeDir      string
}

func New(k8sc *k8s.Client, gitMgr *git.Manager, core EventReporter, e2eHost string, fleetSharedRepoURL, fleetSharedBranch, claudeHomeDir string) *Server {
	return &Server{
		k8sc: k8sc, git: gitMgr, core: core, e2eHost: e2eHost,
		fleetSharedRepoURL: fleetSharedRepoURL, fleetSharedBranch: fleetSharedBranch, claudeHomeDir: claudeHomeDir,
	}
}

// --- e2e sessions (unchanged behavior, k8s-backed instead of Postgres-backed) ---

func (s *Server) KillE2ESession(ctx context.Context, req *agentfleetv1.KillE2ESessionRequest) (*agentfleetv1.KillE2ESessionResponse, error) {
	_, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(req.GetSessionId()))
	if err != nil {
		return nil, err
	}
	killed := false
	if exists {
		if err := s.k8sc.DeleteAll(ctx, req.GetSessionId()); err != nil {
			return nil, err
		}
		slog.Info("grpcserver KillE2ESession", "sessionId", req.GetSessionId())
		killed = true
	}

	var servicesTornDown []string
	if req.GetAlsoTeardownServices() && req.GetRepo() != "" {
		servicesTornDown, err = s.tearDownRepoSharedInstances(ctx, req.GetRepo())
		if err != nil {
			return nil, err
		}
	}

	return &agentfleetv1.KillE2ESessionResponse{Killed: killed, ServicesTornDown: servicesTornDown}, nil
}

// tearDownRepoSharedInstances deletes every shared service instance
// (docs/adr/0034) belonging to repo — human-confirmed opt-in only (the
// dashboard's "kill e2e" checkbox), since a shared instance is keyed by
// repo, not task, and can be in use by other tasks against the same repo
// (that task's own worker pod included).
func (s *Server) tearDownRepoSharedInstances(ctx context.Context, repo string) ([]string, error) {
	instances, err := s.k8sc.ListSharedInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("list shared instances: %w", err)
	}
	var torn []string
	for _, inst := range instances {
		if inst.Repo != repo {
			continue
		}
		if err := s.k8sc.DeleteSharedInstance(ctx, inst.Repo, inst.ServiceKey); err != nil {
			return nil, fmt.Errorf("delete shared instance %s/%s: %w", inst.Repo, inst.ServiceKey, err)
		}
		slog.Info("grpcserver KillE2ESession: tore down shared instance", "repo", inst.Repo, "serviceKey", inst.ServiceKey)
		torn = append(torn, inst.ServiceKey)
	}
	return torn, nil
}

func (s *Server) GetE2ESessionStatus(ctx context.Context, req *agentfleetv1.GetE2ESessionStatusRequest) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	state, exists, err := s.k8sc.GetPod(ctx, k8s.ResourceName(req.GetSessionId()))
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.GetE2ESessionStatusResponse{}, nil
	}
	// Terminating wins over phase: kubelet keeps reporting Running for the
	// whole grace period, and "running" for a pod that is going away is the
	// same lie CreateE2ESession used to tell.
	status := e2eStatusFromPhase(state.Phase)
	podPhase := string(state.Phase)
	if state.Terminating {
		status, podPhase = "terminating", "Terminating"
	}
	return &agentfleetv1.GetE2ESessionStatusResponse{
		Status:        status,
		PreviewUrl:    k8s.PreviewURLFor(s.e2eHost, req.GetSessionId()),
		CodeServerUrl: k8s.CodeServerURLFor(s.e2eHost, req.GetSessionId()),
		StartCmd:      state.StartCmd,
		PodPhase:      podPhase,
		AppReady:      state.AppReady,
		Restarts:      state.Restarts,
		StartedAt:     state.StartedAt,
		// Only when a pod exists — the early return above already covers the
		// no-session case with an empty response. This is how core's own
		// dashboard path (runInE2ePod) reaches a sandbox it did not provision
		// (docs/adr/0045).
		Endpoints: s.endpointsFor(req.GetSessionId()),
	}, nil
}

// CreateE2ESession is idempotent — "safe to call again if one is already
// running" (today's mcpserver docstring, preserved): a GET before create,
// not a create-and-handle-AlreadyExists, since it also needs to report the
// existing session's own status/URL, not just "yes it exists."
func (s *Server) CreateE2ESession(ctx context.Context, req *agentfleetv1.CreateE2ESessionRequest) (*agentfleetv1.CreateE2ESessionResponse, error) {
	name := k8s.ResourceName(req.GetSessionId())
	state, exists, err := s.k8sc.GetPod(ctx, name)
	if err != nil {
		return nil, err
	}
	// A terminating pod is not an existing session. It still exists and
	// still reports Phase=Running for its whole grace period, so the
	// exists-check below used to return its preview URL for a pod that was
	// seconds from vanishing — the caller saw a success and got nothing.
	// "kill_env then request_e2e_env" hits this every time. Wait it out and
	// build a fresh session instead.
	// A pod that reached a terminal phase is not an existing session either
	// (docs/adr/0044). It still reports exists=true and non-Terminating
	// forever, so the check below used to hand back the corpse's preview URL
	// on every subsequent call — and run_command's retry then failed against
	// an unreachable pod for the rest of the session, with nothing GC-ing it.
	// Same remedy as Terminating: get rid of it and build a fresh one.
	dead := exists && (state.Phase == corev1.PodFailed || state.Phase == corev1.PodSucceeded)
	if dead {
		slog.Warn("grpcserver CreateE2ESession: replacing a dead e2e pod",
			"sessionId", req.GetSessionId(), "phase", state.Phase, "detail", state.Detail)
		// Only the pod — Service/Middleware/IngressRoute are create-if-absent
		// (ignoreAlreadyExists), so leaving them keeps the preview URL stable
		// and avoids Traefik churn on every recreate.
		if err := s.k8sc.DeletePod(ctx, req.GetSessionId()); err != nil {
			return nil, fmt.Errorf("delete dead e2e pod for task %s: %w", req.GetSessionId(), err)
		}
	}
	if exists && (state.Terminating || dead) {
		slog.Info("grpcserver CreateE2ESession: waiting for the previous pod to finish terminating", "sessionId", req.GetSessionId())
		if err := s.k8sc.WaitForPodGone(ctx, name, 2*time.Minute); err != nil {
			return nil, fmt.Errorf("previous e2e pod for task %s did not finish terminating: %w", req.GetSessionId(), err)
		}
		exists = false
	}
	if exists {
		// The roster goes out on this path too, not just on creation
		// (docs/adr/0045). A resumed session short-circuits here every time,
		// so returning endpoints only from the create path below would leave
		// exactly the long-lived sessions unable to dial.
		return &agentfleetv1.CreateE2ESessionResponse{
			Status:     e2eStatusFromPhase(state.Phase),
			PreviewUrl: k8s.PreviewURLFor(s.e2eHost, req.GetSessionId()),
			Endpoints:  s.endpointsFor(req.GetSessionId()),
		}, nil
	}

	serviceRefs, extraEnv, err := s.resolveServiceIngredients(ctx, req.GetRepo(), req.GetSessionId(), req.GetServiceIngredients())
	if err != nil {
		return nil, fmt.Errorf("resolve service ingredients: %w", err)
	}
	taskRef := k8s.TaskRef{
		ID: req.GetSessionId(), Repo: req.GetRepo(), StartCmd: req.GetStartCmd(),
		ToolKeys: req.GetToolKeys(), ServiceIngredients: serviceRefs, ExtraEnv: extraEnv,
	}
	if err := s.k8sc.CreatePod(ctx, taskRef); err != nil {
		return nil, fmt.Errorf("create e2e pod: %w", err)
	}
	if err := s.k8sc.CreateService(ctx, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("create e2e service: %w", err)
	}
	// Fenced before it is reachable: the Service above is what makes the MCP
	// ports resolvable, so the policy has to exist by the time anyone can
	// dial them (docs/adr/0045).
	if err := s.k8sc.CreateNetworkPolicy(ctx, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("create e2e networkpolicy: %w", err)
	}
	if err := s.k8sc.CreateMiddleware(ctx, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("create e2e middleware: %w", err)
	}
	if err := s.k8sc.CreateIngressRoute(ctx, s.e2eHost, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("create e2e ingressroute: %w", err)
	}
	slog.Info("grpcserver CreateE2ESession", "sessionId", req.GetSessionId(), "repo", req.GetRepo())
	// "requested", not "running": the pod object exists, but it is Pending —
	// image pull, init containers, scheduling all still ahead of it. Claiming
	// "running" here told the agent the preview was live and then handed it a
	// 502 for the next 10-20 minutes (docs/adr/0044).
	return &agentfleetv1.CreateE2ESessionResponse{
		Status:     "requested",
		PreviewUrl: k8s.PreviewURLFor(s.e2eHost, req.GetSessionId()),
		Endpoints:  s.endpointsFor(req.GetSessionId()),
	}, nil
}

// endpointsFor maps k8s's plain addressing structs onto the wire type — the
// same boundary scopeModeString keeps in the other direction, so the k8s
// package never imports generated code.
//
// Deliberately unconditional: the roster describes where the sandbox *would*
// answer, derived from names, not whether it is answering yet. A caller that
// dials too early gets a connection error and retries, which is a state the
// sidecar's run_command backoff already models (docs/adr/0044). Gating the
// roster on readiness instead would swap that legible failure for an empty
// list meaning both "not ready" and "no such service".
func (s *Server) endpointsFor(taskID string) []*agentfleetv1.ServiceEndpoint {
	resolved := k8s.EndpointsFor(s.k8sc.Namespace, taskID)
	out := make([]*agentfleetv1.ServiceEndpoint, 0, len(resolved))
	for _, e := range resolved {
		out = append(out, &agentfleetv1.ServiceEndpoint{
			Name:     e.Name,
			Address:  e.Address,
			Protocol: e.Protocol,
			Path:     e.Path,
		})
	}
	return out
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
		def, ok := catalog.Services[si.GetKey()]
		if !ok {
			return nil, nil, fmt.Errorf("unknown service ingredient %q", si.GetKey())
		}
		extraEnv = append(extraEnv, corev1.EnvVar{Name: def.EnvVarName, Value: url})
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
	case corev1.PodSucceeded:
		return "exited"
	default:
		// PodUnknown and anything unrecognized. This used to answer
		// "running", which is the one thing it definitely is not known to be.
		return "unknown"
	}
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
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "cloning repo")
	if err := s.git.EnsureRepoCloned(ctx, req.GetRepo(), req.GetRepoUrl()); err != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "clone/fetch failed: "+err.Error())
		return nil, fmt.Errorf("ensure repo cloned: %w", err)
	}
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "adding worktree")
	worktreePath, _, err := s.git.CreateWorktree(ctx, req.GetRepo(), req.GetSessionId(), req.GetBaseBranch())
	if err != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "worktree add failed: "+err.Error())
		return nil, fmt.Errorf("create worktree: %w", err)
	}
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CREATED, "", "")

	// Best-effort, unlike the clone/worktree steps above: stale or
	// momentarily missing fleet-shared skills/context is a degraded worker
	// session, not a broken one, so a sync failure here logs and continues
	// rather than failing the whole dispatch (docs/adr/0032).
	if s.fleetSharedRepoURL != "" {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "syncing fleet-shared skills")
		if err := s.git.SyncFleetShared(ctx, s.fleetSharedRepoURL, s.fleetSharedBranch, s.claudeHomeDir); err != nil {
			slog.Warn("grpcserver: fleet-shared sync failed, continuing with a possibly-stale copy", "sessionId", req.GetSessionId(), "error", err)
		}
	}

	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "minting service credentials")
	serviceRefs, extraEnv, err := s.resolveServiceIngredients(ctx, req.GetRepo(), req.GetSessionId(), req.GetServiceIngredients())
	if err != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "ingredient resolution failed: "+err.Error())
		return nil, fmt.Errorf("resolve service ingredients: %w", err)
	}

	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "creating pod")
	if err := s.k8sc.CreateWorkerPod(ctx, req.GetSessionId(), req.GetRepo(), req.GetLeaseId(), worktreePath, req.GetResumeSessionId(), req.GetResumeFromSeq(), req.GetToolKeys(), serviceRefs, extraEnv); err != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "pod create failed: "+err.Error())
		return nil, fmt.Errorf("create worker pod: %w", err)
	}

	podName := k8s.WorkerResourceName(req.GetSessionId())
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_SCHEDULED, podName, "")
	slog.Info("grpcserver CreateWorkerPod", "sessionId", req.GetSessionId(), "repo", req.GetRepo(), "podName", podName)
	return &agentfleetv1.CreateWorkerPodResponse{PodName: podName}, nil
}

// TearDownSession is opportunistic by design (docs/adr/0020's Consequences
// — core calls this unconditionally on every terminal task status, for
// both kinds, and "nothing to tear down" is a correct, expected no-op, not
// an error).
func (s *Server) TearDownSession(ctx context.Context, req *agentfleetv1.TearDownSessionRequest) (*agentfleetv1.TearDownSessionResponse, error) {
	switch req.GetKind() {
	case agentfleetv1.SessionKind_SESSION_KIND_WORKER:
		return s.tearDownWorker(ctx, req.GetSessionId())
	case agentfleetv1.SessionKind_SESSION_KIND_E2E:
		return s.tearDownE2e(ctx, req.GetSessionId())
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
	slog.Info("grpcserver tearDownWorker", "sessionId", taskID)
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
	slog.Info("grpcserver tearDownE2e", "sessionId", taskID)
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
				SessionId:     info.TaskID,
				Repo:          info.Repo,
				Branch:        info.Branch,
				UpstreamTrack: info.UpstreamTrack,
				MtimeUnix:     info.MtimeUnix,
				Path:          info.Path,
				DirtyFiles:    info.DirtyFiles,
				SizeBytes:     info.SizeBytes,
			})
		}
	}
	// Non-fatal: the worktree list is the point of this call, and a statfs
	// failure (an odd mount, a path that isn't there yet) shouldn't blank the
	// whole page — the dashboard just omits the capacity meter when total is 0.
	total, free, err := s.git.DiskUsage()
	if err != nil {
		slog.Warn("grpcserver ListWorktrees: disk usage unavailable", "error", err)
	}
	return &agentfleetv1.ListWorktreesResponse{Worktrees: out, PvcTotalBytes: total, PvcFreeBytes: free}, nil
}

func (s *Server) DeleteWorktree(ctx context.Context, req *agentfleetv1.DeleteWorktreeRequest) (*agentfleetv1.DeleteWorktreeResponse, error) {
	if err := s.git.DeleteWorktree(ctx, req.GetRepo(), req.GetSessionId(), req.GetAlsoDeleteBranch()); err != nil {
		return nil, err
	}
	return &agentfleetv1.DeleteWorktreeResponse{Deleted: true}, nil
}

func (s *Server) reportEvent(ctx context.Context, taskID string, kind agentfleetv1.SessionKind, phase agentfleetv1.PodPhase, podName, message string) {
	s.core.ReportEvent(ctx, &agentfleetv1.PodEvent{
		SessionId: taskID,
		Kind:      kind,
		Phase:     phase,
		PodName:   podName,
		Message:   message,
	})
}
