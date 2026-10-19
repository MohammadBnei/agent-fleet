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
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/catalog"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
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
	// version is this binary's build, compiled in via -ldflags (see
	// cmd/provisioner/main.go). Reported by GetVersion, and the default ref
	// ListDefinitionFiles reads definitions at.
	version string
	// definitionsRef is FLEET_DEFINITIONS_REF: overrides version as the ref
	// definitions are read at (kind and dev point it at a branch).
	definitionsRef string
	// vault is the Agent Vault client (docs/adr/0065), always wired by main;
	// it fails closed on its own without an agent token. Repo sessions never
	// call it.
	vault *vault.Client
}

// SetVault wires the Agent Vault client (docs/adr/0065), same setter shape as
// SetDefinitionsRef.
func (s *Server) SetVault(v *vault.Client) { s.vault = v }

// SetDefinitionsRef sets FLEET_DEFINITIONS_REF (docs/adr/0062). A setter
// rather than an eighth positional string on New, which already takes six.
func (s *Server) SetDefinitionsRef(ref string) { s.definitionsRef = ref }

// claudeHomeDir is no longer a parameter: fleet-shared is seeded into each
// session's OWN claude-home (git.Manager.ClaudeHomePath), not into one shared
// directory every pod mounted, so there is no single path left to configure.
func New(k8sc *k8s.Client, gitMgr *git.Manager, core EventReporter, e2eHost string, fleetSharedRepoURL, fleetSharedBranch, version string) *Server {
	return &Server{
		k8sc: k8sc, git: gitMgr, core: core, e2eHost: e2eHost,
		fleetSharedRepoURL: fleetSharedRepoURL, fleetSharedBranch: fleetSharedBranch,
		version: version,
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

// SyncRepoCache refreshes one repo's clone cache without creating a pod.
//
// Until this existed, CreateWorkerPod was the only thing that ever advanced
// the cache, so a repo added to the `repos` table stayed uncloned until its
// first session paid for a cold clone, and an idle repo's cache went stale
// with nothing able to touch it.
//
// repo_url is trusted here because core resolved it from the `repos` table —
// the same rule ProvisionService follows for repo. The provisioner has no
// database to check it against.
func (s *Server) SyncRepoCache(ctx context.Context, req *agentfleetv1.SyncRepoCacheRequest) (*agentfleetv1.SyncRepoCacheResponse, error) {
	// repo is joined into a filesystem path (git.Manager.repoPath), so an
	// empty one would name the whole cache directory rather than a repo.
	if req.GetRepo() == "" || req.GetRepoUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "repo and repo_url are required")
	}
	started := time.Now()
	res, err := s.git.EnsureRepoCloned(ctx, req.GetRepo(), req.GetRepoUrl())
	if err != nil {
		// Returned verbatim: git.Manager.run already put git's own stderr in
		// here, and that line is the entire diagnostic the operator gets.
		return nil, fmt.Errorf("sync repo cache %q: %w", req.GetRepo(), err)
	}
	slog.Info("grpcserver SyncRepoCache", "repo", req.GetRepo(), "cloned", res.Cloned, "advanced", res.Advanced, "head", res.Head)
	return &agentfleetv1.SyncRepoCacheResponse{
		Cloned:          res.Cloned,
		Head:            res.Head,
		CommitsAdvanced: int32(res.Advanced),
		HeadChanged:     res.Changed,
		DurationMs:      time.Since(started).Milliseconds(),
	}, nil
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
	// docs/adr/0064: a profile switches the pod to the definition profile.
	// Checked before anything is reported or touched.
	var definition *k8s.DefinitionProfile
	if d := req.GetDefinition(); d != nil {
		if d.GetName() == "" || d.GetGitAuthorName() == "" || d.GetGitAuthorEmail() == "" {
			return nil, status.Error(codes.InvalidArgument, "definition profile needs a name and a git identity")
		}
		if len(req.GetToolKeys()) > 0 {
			return nil, status.Error(codes.InvalidArgument, "a definition session never gets tool keys")
		}
		// The definition's worker-side hardening (FLEET_DEFINITION) needs this
		// release's worker, so only the provisioner's default image runs one.
		if req.GetImage() != "" {
			return nil, status.Error(codes.InvalidArgument, "a definition session runs the default worker image only")
		}
		// docs/adr/0065: an older core sends neither, and its warm fails here.
		if d.GetVault() == nil || d.GetVault().GetName() != d.GetName() || d.GetTimeoutSeconds() <= 0 {
			return nil, status.Error(codes.InvalidArgument, "definition profile needs its own vault and a timeout (docs/adr/0065)")
		}
		definition = &k8s.DefinitionProfile{Name: d.GetName(), GitAuthorName: d.GetGitAuthorName(), GitAuthorEmail: d.GetGitAuthorEmail()}
	}
	// A definition pod's clone reads the cache's origin/<base> and never
	// contacts the remote, so its fetch is load-bearing and never skipped.
	ensureCache := s.git.EnsureRepoCachedForSession
	if definition != nil {
		ensureCache = s.git.EnsureRepoFetched
	}
	// Reported immediately, before any git/k8s work starts — otherwise a
	// claimed task with a hung or merely slow provisioning step is
	// indistinguishable from one the provisioner never received at all
	// (confirmed live: a task sat claimed for 20+ minutes with zero pod
	// events either way). PROVISIONING's `message` carries the current
	// sub-step rather than a new enum value per step, since these are
	// provisioner-internal detail, not states any other component branches
	// on — see core.proto's own comment on the enum value.
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "cloning repo")
	if err := ensureCache(ctx, req.GetRepo(), req.GetRepoUrl()); err != nil {
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
	//
	// Never for a definition pod (docs/adr/0064): fleet-shared's settings.json
	// pre-approves arbitrary-exec Bash (`go run`, `bun x`, ...) that would never
	// reach canUseTool. It gets the image's baked plugins until P2's genome.
	// Fatal, not best-effort: a copy an older replica left behind is removed,
	// or no pod is built.
	if definition != nil {
		if err := s.git.RemoveFleetShared(req.GetSessionId()); err != nil {
			s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "claude-home setup failed: "+err.Error())
			return nil, fmt.Errorf("remove fleet-shared: %w", err)
		}
	} else if s.fleetSharedRepoURL != "" {
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

	// docs/adr/0065: the vault matches the pinned spec, the CA is published,
	// and this pod's token is minted, all before the pod exists. Any failure
	// (a missing secret, no vault, no agent token) refuses the warm here.
	var label string
	if definition != nil {
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_PROVISIONING, "", "minting vault token")
		token, l, deadline, err := s.mintDefinitionToken(ctx, req.GetSessionId(), req.GetDefinition())
		if err != nil {
			s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", status.Convert(err).Message())
			return nil, err
		}
		label, definition.ProxyURL, definition.DeadlineSeconds, definition.VaultLabel = l, s.vault.ProxyURL(token, definition.Name), deadline, l
	}

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
		Image:         req.GetImage(),
		ServiceRefs:   nil,
		ExtraEnv:      nil,
		Definition:    definition,
	}); err != nil {
		if definition != nil {
			if rerr := s.vault.Revoke(ctx, definition.Name, label); rerr != nil {
				slog.Warn("grpcserver CreateWorkerPod: revoking the token of a pod never created (its TTL bounds it)", "sessionId", req.GetSessionId(), "error", rerr)
			}
		}
		s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_CRASHED, "", "pod create failed: "+err.Error())
		return nil, fmt.Errorf("create worker pod: %w", err)
	}

	podName := k8s.WorkerResourceName(req.GetSessionId())
	s.reportEvent(ctx, req.GetSessionId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER, agentfleetv1.PodPhase_POD_PHASE_SCHEDULED, podName, "")
	// Same resolution the pod spec just used, so core records the build this
	// session is actually running rather than whatever the defaults become.
	workerImage, sidecarImage := s.k8sc.ResolveImages(req.GetImage())
	slog.Info("grpcserver CreateWorkerPod", "sessionId", req.GetSessionId(), "repo", req.GetRepo(), "podName", podName,
		"workerImage", workerImage, "sidecarImage", sidecarImage)
	return &agentfleetv1.CreateWorkerPodResponse{
		PodName:                  podName,
		WorkerImage:              workerImage,
		SidecarImage:             sidecarImage,
		DefinitionProfileApplied: definition != nil,
		VaultTokenApplied:        definition != nil,
	}, nil
}

// mintDefinitionToken reconciles the definition's vault, publishes the vault
// CA and mints this pod's token. The pod's deadline is max(MinTTL, timeout); the
// token lives 60s longer (activeDeadlineSeconds counts from the Job's
// startTime, set at create: ms of skew).
func (s *Server) mintDefinitionToken(ctx context.Context, sessionID string, d *agentfleetv1.DefinitionPodProfile) (token, label string, deadline int64, err error) {
	if err := s.vault.Reconcile(ctx, vaultSpec(d.GetVault())); err != nil {
		return "", "", 0, status.Error(codes.FailedPrecondition, "vault: "+err.Error())
	}
	pem, err := s.vault.CA(ctx)
	if err == nil {
		err = s.k8sc.EnsureVaultCA(ctx, pem)
	}
	if err != nil {
		return "", "", 0, status.Error(codes.Unavailable, "vault CA: "+err.Error())
	}
	deadline = max(int64(vault.MinTTL.Seconds()), d.GetTimeoutSeconds())
	label = sessionID + "." + strconv.FormatInt(time.Now().UnixMilli(), 10)
	token, err = s.vault.Mint(ctx, d.GetName(), label, time.Duration(deadline+60)*time.Second)
	if err != nil {
		return "", "", 0, status.Error(codes.Unavailable, err.Error())
	}
	return token, label, deadline, nil
}

func vaultSpec(v *agentfleetv1.VaultSpec) vault.Spec {
	out := vault.Spec{Name: v.GetName()}
	for _, svc := range v.GetServices() {
		out.Services = append(out.Services, vault.Service{Host: svc.GetHost(), Paths: svc.GetPaths(), Secret: svc.GetSecret()})
	}
	return out
}

// ReconcileVaults makes every listed vault match its spec and empties every
// other vault this provisioner administers (docs/adr/0065 §2): the request is
// core's full desired set. Errors are per vault, never a value.
func (s *Server) ReconcileVaults(ctx context.Context, req *agentfleetv1.ReconcileVaultsRequest) (*agentfleetv1.ReconcileVaultsResponse, error) {
	out := &agentfleetv1.ReconcileVaultsResponse{}
	keep := map[string]bool{}
	for _, v := range req.GetVaults() {
		keep[v.GetName()] = true
		res := &agentfleetv1.VaultResult{Name: v.GetName()}
		if err := s.vault.Reconcile(ctx, vaultSpec(v)); err != nil {
			res.Error = err.Error()
		}
		out.Results = append(out.Results, res)
	}
	if err := s.vault.Prune(ctx, keep); err != nil {
		out.PruneError = err.Error()
	}
	return out, nil
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
	job, exists, err := s.k8sc.GetWorkerJob(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &agentfleetv1.TearDownSessionResponse{TornDown: false}, nil
	}
	// docs/adr/0065: a definition pod's token dies before the pod, exactly
	// that token (the label is on the Job). Best-effort (own 3s budget): its
	// TTL bounds a miss, and a Job already gone was revoked when it went
	// terminal (reconcile) or never got one. A repo session never calls the
	// vault.
	if job.Definition != "" && job.VaultLabel != "" {
		if err := s.vault.Revoke(ctx, job.Definition, job.VaultLabel); err != nil {
			slog.Warn("grpcserver tearDownWorker: vault revoke failed, the token's TTL bounds it", "sessionId", taskID, "error", err)
		}
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

// GetVersion answers "what build is the provisioner, and what will it stamp
// onto the next pod?". Process-local constants only — no Kubernetes call, so
// the dashboard's 5s topology poll can ask on every tick.
func (s *Server) GetVersion(_ context.Context, _ *agentfleetv1.GetVersionRequest) (*agentfleetv1.GetVersionResponse, error) {
	worker, sidecar := s.k8sc.ResolveImages("")
	return &agentfleetv1.GetVersionResponse{
		Version:      s.version,
		WorkerImage:  worker,
		SidecarImage: sidecar,
	}, nil
}

// ListDefinitionFiles is core's definitions sync source (docs/adr/0062). The
// ref defaults to this provisioner's own release tag, so definitions ship with
// the core release that judges them instead of going live on merge to main.
// Fails closed: no source, no ref or any read error is an error.
func (s *Server) ListDefinitionFiles(ctx context.Context, _ *agentfleetv1.ListDefinitionFilesRequest) (*agentfleetv1.ListDefinitionFilesResponse, error) {
	if s.fleetSharedRepoURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "FLEET_SHARED_REPO_URL is unset: no definitions source")
	}
	ref := s.definitionsRef
	if ref == "" {
		if s.version == "dev" || s.version == "" {
			return nil, status.Error(codes.FailedPrecondition, "a dev build has no release tag to read definitions at: set FLEET_DEFINITIONS_REF")
		}
		ref = s.version
	}
	files, sha, err := s.git.ReadDefinitions(ctx, s.fleetSharedRepoURL, ref)
	if err != nil {
		// git.Manager.run puts git's own stderr in here: the whole diagnostic.
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	out := &agentfleetv1.ListDefinitionFilesResponse{Sha: sha}
	for p, b := range files {
		out.Files = append(out.Files, &agentfleetv1.DefinitionFile{Path: p, Content: b})
	}
	return out, nil
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
