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
}

// claudeHomeDir is no longer a parameter: fleet-shared is seeded into each
// session's OWN claude-home (git.Manager.ClaudeHomePath), not into one shared
// directory every pod mounted, so there is no single path left to configure.
func New(k8sc *k8s.Client, gitMgr *git.Manager, core EventReporter, e2eHost string, fleetSharedRepoURL, fleetSharedBranch string) *Server {
	return &Server{
		k8sc: k8sc, git: gitMgr, core: core, e2eHost: e2eHost,
		fleetSharedRepoURL: fleetSharedRepoURL, fleetSharedBranch: fleetSharedBranch,
	}
}



// --- what replaced the sandbox (docs/adr/0048 §6) ---

// ExposeSession publishes a port from a session's pod at a public URL.
//
// Idempotent, and that is a requirement rather than a nicety: the agent is
// told it may call expose() again after a warm, and a session's pod changes
// identity on every warm while its hostname must not.
func (s *Server) ExposeSession(ctx context.Context, req *agentfleetv1.ExposeSessionRequest) (*agentfleetv1.ExposeSessionResponse, error) {
	url, err := s.k8sc.ExposeSession(ctx, req.GetSessionId(), req.GetPort(), s.e2eHost)
	if err != nil {
		return nil, fmt.Errorf("expose session: %w", err)
	}
	slog.Info("grpcserver ExposeSession", "sessionId", req.GetSessionId(), "port", req.GetPort(), "url", url)
	return &agentfleetv1.ExposeSessionResponse{Url: url}, nil
}

func (s *Server) UnexposeSession(ctx context.Context, req *agentfleetv1.UnexposeSessionRequest) (*agentfleetv1.UnexposeSessionResponse, error) {
	if err := s.k8sc.UnexposeSession(ctx, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("unexpose session: %w", err)
	}
	slog.Info("grpcserver UnexposeSession", "sessionId", req.GetSessionId())
	return &agentfleetv1.UnexposeSessionResponse{}, nil
}

// ProvisionService is what is left of the ingredient system: a shared
// Postgres or Redis, keyed by repo, minted on demand.
//
// Reuses MintServiceCredentials rather than growing a parallel path — the
// scope rules (ADR-0034) are unchanged and were never the part that was
// wrong. What changed is who decides a service is needed: the agent asks for
// one when it turns out to need it, instead of a profile table declaring it
// up front for every session on the repo.
func (s *Server) ProvisionService(ctx context.Context, req *agentfleetv1.ProvisionServiceRequest) (*agentfleetv1.ProvisionServiceResponse, error) {
	if _, ok := catalog.Services[req.GetKind()]; !ok {
		return nil, fmt.Errorf("unknown service %q", req.GetKind())
	}
	dsn, err := s.k8sc.MintServiceCredentials(ctx, req.GetRepo(), req.GetSessionId(), req.GetKind(), k8s.ScopeModeTaskScoped)
	if err != nil {
		return nil, fmt.Errorf("mint %s credentials: %w", req.GetKind(), err)
	}
	slog.Info("grpcserver ProvisionService", "sessionId", req.GetSessionId(), "repo", req.GetRepo(), "kind", req.GetKind())
	return &agentfleetv1.ProvisionServiceResponse{Dsn: dsn}, nil
}

// SweepSession reclaims a session's disk — both halves, which live on
// different volumes: the working-tree PVC and the SDK state directory.
//
// Both deletes are attempted even if the first fails, and the error is
// reported afterwards. Returning early would leave core unable to mark the
// session swept, so the next GC pass would retry the half that already
// succeeded — harmless, but it would also never make progress on the other.
func (s *Server) SweepSession(ctx context.Context, req *agentfleetv1.SweepSessionRequest) (*agentfleetv1.SweepSessionResponse, error) {
	pvcErr := s.k8sc.DeleteSessionPVC(ctx, req.GetSessionId())
	dirErr := s.git.DeleteSessionDir(req.GetSessionId())
	if pvcErr != nil {
		return nil, fmt.Errorf("sweep session pvc: %w", pvcErr)
	}
	if dirErr != nil {
		return nil, fmt.Errorf("sweep session dir: %w", dirErr)
	}
	slog.Info("grpcserver SweepSession", "sessionId", req.GetSessionId())
	return &agentfleetv1.SweepSessionResponse{}, nil
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
	// No worktree step. The session's working tree is cloned by an init
	// container in the pod itself, because it lives on a per-session
	// local-path PVC this process never mounts (docs/adr/0048 §4/§5) — a
	// worktree added here would land on a different volume, silently.
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CREATED, "", "")

	// Fatal, and deliberately outside the fleet-shared block below: the pod
	// cannot start at all without a writable claude-home (see
	// git.EnsureClaudeHome), so failing here beats creating a pod that will
	// crash-loop on its entrypoint's plugin copy.
	if err := s.git.EnsureClaudeHome(req.GetSessionId()); err != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "claude-home setup failed: "+err.Error())
		return nil, fmt.Errorf("ensure claude home: %w", err)
	}

	// Best-effort, unlike the clone/worktree steps above: stale or
	// momentarily missing fleet-shared skills/context is a degraded worker
	// session, not a broken one, so a sync failure here logs and continues
	// rather than failing the whole dispatch (docs/adr/0032).
	//
	// Seeded into this session's OWN claude-home rather than one directory
	// shared by every pod. That is what removes the rsync --delete race
	// docs/adr/0048 §4 describes: the old layout rewrote settings.json and
	// skills/ under other sessions mid-turn, with no lock. Here it is written
	// once, before the pod exists, and nothing touches it again while the
	// session is live.
	if s.fleetSharedRepoURL != "" {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "syncing fleet-shared skills")
		if err := s.git.SyncFleetShared(ctx, s.fleetSharedRepoURL, s.fleetSharedBranch, s.git.ClaudeHomePath(req.GetSessionId())); err != nil {
			slog.Warn("grpcserver: fleet-shared sync failed, continuing with a possibly-stale copy", "sessionId", req.GetSessionId(), "error", err)
		}
	}

	// No service ingredients to resolve up front. A session gets a Postgres
	// when it asks for one via request_service, not because a profile row
	// declared that every session on this repo might want one — which is what
	// made a shared instance the fleet's problem to mint, size and tear down
	// on behalf of sessions that often never touched it (docs/adr/0048 §6).

	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "creating pod")
	if err := s.k8sc.CreateWorkerPod(ctx, k8s.WorkerPodSpec{
		SessionID:     req.GetSessionId(),
		Repo:          req.GetRepo(),
		RepoURL:       req.GetRepoUrl(),
		BaseBranch:    req.GetBaseBranch(),
		LeaseID:       req.GetLeaseId(),
		ResumeID:      req.GetResumeSessionId(),
		ResumeFromSeq: req.GetResumeFromSeq(),
		ToolKeys:      req.GetToolKeys(),
		ServiceRefs:   nil,
		ExtraEnv:      nil,
	}); err != nil {
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
	// `kind` is no longer branched on. It distinguished a worker pod from an
	// e2e sandbox, and there is no sandbox (docs/adr/0048 §6) — a session has
	// exactly one pod. The field stays on the wire so a rolling deploy where
	// one side is older still parses; it just selects nothing.
	return s.tearDownWorker(ctx, req.GetSessionId())
}

// tearDownWorker deletes the Job and nothing else. It deliberately does not
// touch the session's disk: a stopped session must stay resumable, and the
// only thing that reclaims its PVC and SDK state is the retention GC, through
// SweepSession.
//
// It has never touched git state either (reliability-findings.md #2) — an
// earlier version ran an unconditional `branch -D` here and destroyed the only
// reference to a never-pushed branch's commits whenever teardown followed a
// failed push. There is no branch for it to delete now in any case.
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
	// Whatever this session was publishing stops being published. Best-effort
	// and unconditional: most sessions never call expose(), so "nothing to
	// remove" is the common case, not a failure.
	if err := s.k8sc.UnexposeSession(ctx, taskID); err != nil {
		slog.Warn("grpcserver tearDownWorker: unexpose failed", "sessionId", taskID, "error", err)
	}
	slog.Info("grpcserver tearDownWorker", "sessionId", taskID)
	return &agentfleetv1.TearDownSessionResponse{TornDown: true}, nil
}

// ListWorkerPods reports which sessions Kubernetes says have a live worker
// Job. Core reconciles pod_phase against this every 60s — the safety net that
// replaces the deleted heartbeat (docs/adr/0048).
//
// A pushed pod event can be dropped, most plausibly while core itself is
// restarting, which is precisely when a deploy is under way. A dropped
// TERMINATED would leave a finished session holding a concurrency slot
// forever; reconciling against this makes that self-heal in a minute instead
// of never.
func (s *Server) ListWorkerPods(ctx context.Context, _ *agentfleetv1.ListWorkerPodsRequest) (*agentfleetv1.ListWorkerPodsResponse, error) {
	jobs, err := s.k8sc.ListWorkerJobsByLabel(ctx)
	if err != nil {
		return nil, fmt.Errorf("list worker jobs: %w", err)
	}
	out := make([]*agentfleetv1.LiveWorkerPod, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, &agentfleetv1.LiveWorkerPod{
			SessionId: j.TaskID,
			PodName:   j.JobName,
			Phase:     j.Phase,
		})
	}
	return &agentfleetv1.ListWorkerPodsResponse{Pods: out}, nil
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
