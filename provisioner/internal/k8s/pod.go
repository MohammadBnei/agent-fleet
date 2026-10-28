package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
)

// workerJobTTLSeconds is how long a finished worker Job (and the pod it
// owns) sticks around before Kubernetes' TTL controller garbage-collects
// it — reliability-findings.md #11: this replaces the reconcile loop's own
// hand-rolled terminal-phase GC pass. Long enough to `kubectl logs` a
// crashed worker, short enough not to accumulate.
//
// 5 minutes turned out not to be long enough for the first half of that, and
// it never even applied: the reconcile loop deleted a terminal Job within
// ~60s (pod died 19:24:14, DeleteWorkerJob 19:24:22 on 2026-08-17), so the
// pod was gone before kube-state-metrics ever scraped it. The fleet's own
// record of *why* a worker died was therefore empty —
// kube_pod_container_status_last_terminated_reason had no worker rows at all,
// and reading the node's dmesg over SSH was the only way to establish an
// OOMKill. The loop no longer deletes a Failed Job (see
// reconcile.gcTerminalWorkerJobs); this is what reaps it instead, and 30
// minutes is a window a human can actually act inside.
const workerJobTTLSeconds = 1800

// claudeConfigDir redirects the Agent SDK's session-state directory
// (normally $HOME/.claude, which is a fresh container filesystem every
// pod) onto the shared workspace PVC — without this, `resume:` has
// nothing to resume from regardless of what session id is passed
// (sessions redesign, supersedes docs/adr/0021/0025's phase-boundary
// framing; the redirect itself was originally described in ADR-0016 but
// never actually implemented until now).
//
// Deliberately NOT under /workspace, and that is a correctness rule rather
// than a preference: /workspace is the git clone the agent is told to commit
// and push. Nested there, a plain `git add -A` sweeps full conversation
// transcripts and the SDK's credentials into a PR, and a `git clean -xfd` — an
// ordinary reset move — destroys the state that makes the session resumable.
// Neither is a mistake the agent could be blamed for; the directory simply
// must not live inside the tree it is being asked to commit.
//
// Per-session isolation comes from the MOUNT (subPath claude-home/<id>), not
// from cwd. It used to come from cwd, when each task had its own worktree path
// and the SDK derived its per-project subdirectory from that path (every
// non-alphanumeric character replaced with `-`, confirmed against the bundled
// cli.js — not a hash). Every session's cwd is /workspace now, so a shared
// root would land all of them in the same `projects/-workspace/`.
const claudeConfigDir = "/claude-home"

// genomeDir is where the worker finds its context packs (docs/adr/0063); the
// worker passes it as an additionalDirectory.
const genomeDir = "/genome"

// sessionCacheDir is the per-session dependency cache mount (docs/adr/0048 §4).
const sessionCacheDir = "/cache"

// browsersDir is where Playwright's browser builds are mounted, read-only,
// from the shared PVC — and PLAYWRIGHT_BROWSERS_PATH is what makes Playwright
// look there instead of in the image.
//
// They used to be baked into worker/Dockerfile: 2.0 GB of the image's 5.13 GB,
// carried by every session on every node whether or not it ever opened a page,
// and re-pulled on every image bump. They are large, version-pinned and never
// written at runtime, which is exactly the shape a read-only shared mount is
// for — the same shape /repo-cache already uses.
//
// Read-only is a real boundary, not decoration: one session cannot corrupt the
// browser build every other session launches. Verified by driving a real
// browser_navigate against a read-only mount before this was written.
//
// Populated by the browser-cache Job (browsercache.go), not by any worker.
const browsersDir = "/ms-playwright"
const browsersSubPath = "browsers"

// cacheEnv points every package manager's cache at the /cache mount.
//
// Without these the mount is real and empty: bun, Go and npm all default to
// somewhere under $HOME, which is the container's ephemeral layer and dies with
// the pod. So the volume survived a warm holding nothing, and every warm paid
// for a cold install — the exact cost the storage split exists to remove. A
// mounted directory nobody writes to is not a cache.
//
// Deliberately broad rather than "the languages we use today": a target repo is
// whatever a human onboards, and the failure mode of a missing entry here is
// silent slowness that nobody attributes to this file.
func cacheEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "BUN_INSTALL_CACHE_DIR", Value: sessionCacheDir + "/bun"},
		{Name: "GOMODCACHE", Value: sessionCacheDir + "/go/mod"},
		{Name: "GOCACHE", Value: sessionCacheDir + "/go/build"},
		{Name: "npm_config_cache", Value: sessionCacheDir + "/npm"},
		{Name: "npm_config_store_dir", Value: sessionCacheDir + "/pnpm"},
		{Name: "YARN_CACHE_FOLDER", Value: sessionCacheDir + "/yarn"},
		{Name: "UV_CACHE_DIR", Value: sessionCacheDir + "/uv"},
		{Name: "PIP_CACHE_DIR", Value: sessionCacheDir + "/pip"},
		{Name: "CARGO_HOME", Value: sessionCacheDir + "/cargo"},
		// Catch-all for everything not named above that respects the XDG spec.
		{Name: "XDG_CACHE_HOME", Value: sessionCacheDir + "/xdg"},
	}
}

// contextBudgetEnv tunes the Agent SDK's own context-management machinery for
// the fleet's usage pattern (ADR-0046). It is read directly by the bundled
// Claude Code and does not exist as an `Options` field, so env is the only way
// to set it.
//
// It used to set four variables. Three — USE_API_CLEAR_TOOL_USES,
// API_MAX_INPUT_TOKENS, API_TARGET_INPUT_TOKENS — were dropped when the worker
// moved to Agent SDK 0.3.233, because they no longer exist in it: all three are
// present as plain strings in 0.1.77's cli.js and absent from 0.3.233's native
// binary, and 0.3.233's nearest successor (USE_API_CONTEXT_MANAGEMENT) is read
// and then ANDed with a literal false, so no env var turns server-side context
// editing on any more. Setting them was not harmful, it was nothing — which is
// worse, because the pod spec still read like a tuned session.
//
// ADR-0046's premise had expired independently: it was written because
// `run_command` (an MCP tool, which microcompaction never touches) carried the
// fleet's largest context load, and docs/adr/0048 §6 deleted `run_command` —
// builds run through Bash now, which microcompaction does cover.
//
// Re-check this list on every SDK bump. Nothing here is typed, nothing fails
// when a variable disappears, and no test can see it. What found the three dead
// ones: env names are plain greppable strings inside the SDK's native binary,
// so grep each name in
// worker/node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude — and
// grep a name known to be there first (ANTHROPIC_MODEL, MAX_THINKING_TOKENS),
// so that zero hits means removal rather than a bad grep.
func contextBudgetEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		// Per-result ceiling for MCP tools. The CLI's default is 25000
		// tokens (~100KB), which is bounded but far too generous when a
		// single verbose test run can hit it and then stay in context
		// permanently. 10000 still fits a real stack trace.
		{Name: "MAX_MCP_OUTPUT_TOKENS", Value: "10000"},
	}
}

// workerMaxCPU/workerMaxMemory are the worker container's limits. They must
// stay <= limitRange.max in k8s/core.yaml — that LimitRange is namespace-wide,
// so exceeding it means the pod is rejected at admission and never exists,
// rather than merely being throttled. Named constants so the test that pins
// the two files together has something to assert on.
//
// These were e2eMaxCPU/e2eMaxMemory until docs/adr/0048 §6 deleted the pod
// they sized, taking their guard test with them; the worker is what runs
// builds now, so it is what the ceiling has to fit.
const (
	workerMaxCPU    = "4000m"
	workerMaxMemory = "4Gi"
)

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// CreatePod and its e2e-preview machinery used to fill most of this file:
// a bare long-running Pod, its Service, its IngressRoute, its stripPrefix
// Middleware, its per-task NetworkPolicy, a readiness probe on the app port
// and a resource envelope pinned against the namespace LimitRange.
//
// All of it is deleted (docs/adr/0048 §6). A session has one pod, and what it
// publishes is one Service and one route created on demand by expose().
//
// CreateWorkerPod builds that pod — worker + sidecar (docs/adr/0020 point 5),
// plus the clone init container — wrapped in a batch/v1.Job
// (reliability-findings.md #11) rather than a bare Pod, so terminal-state GC
// comes from Job semantics instead of a hand-rolled sweep. BackoffLimit is
// deliberately 0: a crashed session is a human's decision to warm again, and a
// k8s-level retry would restart an agent mid-conversation with no one asking.
//
// WorkerPodSpec is what a session's pod needs to exist. A struct rather than
// ten positional parameters, which is what this was — and which is how
// `worktreePath` sat in the middle of the list right up until the volume it
// pointed at stopped existing.
type WorkerPodSpec struct {
	SessionID string
	Repo      string
	// RepoURL/BaseBranch are for the clone init container, not for this
	// process: the working tree is cloned inside the pod now (docs/adr/0048
	// §5), because it lives on a volume the provisioner never mounts.
	RepoURL       string
	BaseBranch    string
	LeaseID       string
	ResumeID      string
	ResumeFromSeq int64
	ToolKeys      []string
	// Image is repos.image — the worker container's image for this repo, ''
	// meaning the fleet default (docs/adr/0048 §6 replaced four toolchain
	// ingredients with this one column). Deliberately NOT applied to the
	// clone init container or the sidecar: see workerImage below.
	Image       string
	ServiceRefs []ServiceIngredientRef
	ExtraEnv    []corev1.EnvVar
	// Definition switches the pod to the definition profile (docs/adr/0064).
	Definition *DefinitionProfile
	// RepoVault puts a repo session's pod behind its owner's vault
	// (docs/adr/0075 §7). The provisioner always sets it; nil (tests only)
	// leaves the pod with no credential at all.
	RepoVault *RepoVault
}

// RepoVault is the vault half of the definition profile, for a repo session:
// its owner's vault, open egress, the commit identity from config.
type RepoVault struct {
	Vault           string // vault.OwnerVault(owner)
	ProxyURL        string
	DeadlineSeconds int64
	VaultLabel      string
	GitAuthorName   string
	GitAuthorEmail  string
}

// VaultOwnerLabel marks a repo session's pod with its owner vault. The
// vault's NetworkPolicy admits pods carrying the key (infra-bootstrap #358).
const VaultOwnerLabel = "fleet.bnei.dev/vault-owner"

// DefinitionProfile is what a definition session's pod needs beyond the
// ordinary spec, built by core from the pinned definition version.
type DefinitionProfile struct {
	Name           string
	GitAuthorName  string
	GitAuthorEmail string
	// ProxyURL is HTTPS_PROXY, carrying this pod's vault session token: the
	// only credential in the pod, and one that reaches only its vault's rules
	// (docs/adr/0065). Empty refuses the pod.
	ProxyURL string
	// DeadlineSeconds is the Job's activeDeadlineSeconds: the fixed lifetime
	// cap (grpcserver's podLifetime). The token outlives it by 60s.
	DeadlineSeconds int64
	// VaultLabel is the token's vault session label, kept on the Job
	// (VaultLabelAnnotation) so every revoke path deletes exactly this
	// pod's token. Not a secret.
	VaultLabel string
}

// VaultLabelAnnotation carries DefinitionProfile.VaultLabel on the Job.
const VaultLabelAnnotation = "fleet.bnei.dev/vault-label"

// VaultPlaceholder stands in for every model and GitHub credential in a
// definition pod: the vault overwrites the whole Authorization header.
const VaultPlaceholder = "fleet-vault-placeholder"

// VaultCAConfigMap holds the vault's MITM root (key ca.pem) and, for repo
// pods' direct TLS, the system roots plus that root (key bundle.pem), written
// by the provisioner (EnsureVaultCA) so the worker fetches nothing.
const VaultCAConfigMap = "agent-vault-ca"

const vaultCADir = "/etc/agent-vault"

// repoCacheMount is the read-only clone-cache mount every container gets. A
// definition pod sees only its own repo, at the same absolute path, so the
// --shared clone's alternates (/repo-cache/<repo>/.git/objects) still resolve.
func repoCacheMount(spec WorkerPodSpec) corev1.VolumeMount {
	if spec.Definition != nil {
		return corev1.VolumeMount{Name: "shared", MountPath: "/repo-cache/" + spec.Repo, SubPath: "repos/" + spec.Repo, ReadOnly: true}
	}
	return corev1.VolumeMount{Name: "shared", MountPath: "/repo-cache", SubPath: "repos", ReadOnly: true}
}

// definitionWorkerEnv narrows the worker's env for a definition pod
// (docs/adr/0064, docs/adr/0065). No LEASE_ID: the sidecar holds the lease and
// is the only thing that talks to core. No real credential:
// CLAUDE_CODE_OAUTH_TOKEN and GH_TOKEN are placeholders the vault overwrites
// on the wire. The identity comes from the reviewed definition file, and the
// worker skips its gh auth setup (`gh api user`, gh auth setup-git and the
// safe.directory line) because FLEET_DEFINITION is set, NOT because GH_TOKEN is
// empty: it is not, and a `gh api user` with the placeholder matches no vault
// rule, so every definition session would fail at start. safe.directory=
// /workspace comes through GIT_CONFIG_* instead (the sidecar's mechanism, not
// its '*' value). FLEET_DEFINITION also closes Claude Code's own cross-session
// channel (docs/adr/0060), which never touches core and so is not bounded by
// the definition allowlist.
//
// The proxy, its CA and NO_PROXY (loopback for the sidecar, core, the vault
// API) are set here: both cases of HTTPS_PROXY (curl prefers lowercase),
// NODE_USE_ENV_PROXY (Node's own fetch ignores the proxy vars without it), and
// the CA in every variable a client in the worker image reads. No HTTP_PROXY:
// the vault's plain http:// path injects too, so a plain-http request goes
// direct and the egress lock drops it. No .cluster.local in NO_PROXY: such a
// request goes to the proxy, which denies private ranges, and the egress lock
// would drop it direct anyway (docs/adr/0065 §4, both departures recorded).
func definitionWorkerEnv(env []corev1.EnvVar, d DefinitionProfile, coreHost string) []corev1.EnvVar {
	noProxy := []string{"127.0.0.1", "localhost"}
	if coreHost != "" {
		noProxy = append(noProxy, coreHost)
	}
	return vaultWorkerEnv(env, d.ProxyURL, noProxy, vaultCADir+"/ca.pem", d.Name, d.GitAuthorName, d.GitAuthorEmail)
}

// withGitURLRewrite appends `url.<rewrite>.insteadOf = https://github.com/` to
// the worker's GIT_CONFIG_* (docs/adr/0080 §2): the agent's own push reads the
// clone's github.com origin. The session id is the URL's user name, so the
// local forge, which challenges a push and takes any credentials, records
// which session pushed; GIT_TERMINAL_PROMPT=0 means git needs the password in
// the URL too.
func withGitURLRewrite(env []corev1.EnvVar, rewrite, sessionID string) []corev1.EnvVar {
	u, err := url.Parse(rewrite)
	if err != nil {
		return env // config.Load refuses a malformed rewrite, so this is not reached from a Config
	}
	u.User = url.UserPassword(sessionID, "x")
	n, found := 0, false
	for i := range env {
		if env[i].Name == "GIT_CONFIG_COUNT" {
			n, _ = strconv.Atoi(env[i].Value)
			env[i].Value, found = strconv.Itoa(n+1), true
		}
	}
	if !found {
		env = append(env, corev1.EnvVar{Name: "GIT_CONFIG_COUNT", Value: "1"})
	}
	return append(env,
		corev1.EnvVar{Name: fmt.Sprintf("GIT_CONFIG_KEY_%d", n), Value: "url." + u.String() + ".insteadOf"},
		corev1.EnvVar{Name: fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), Value: "https://github.com/"},
	)
}

// repoNoProxy is what a repo pod reaches directly (docs/adr/0075 §7, spike
// PR4a): the cluster, the LAN and the split-horizon names. The vault refuses
// private ranges, so these would fail through it. The apex bnei.dev too:
// Go's matcher reads ".bnei.dev" as subdomains only. Hostnames as well as CIDRs:
// bun and python ignore CIDR entries. The CIDRs are the live cluster's
// (kubespray: services 10.233.0.0/18, pods 10.233.64.0/18).
var repoNoProxy = []string{".cluster.local", ".svc", ".bnei.lan", ".bnei.dev", "bnei.dev", "10.233.0.0/18", "10.233.64.0/18", "192.168.1.0/24"}

// repoWorkerEnv is definitionWorkerEnv for a repo pod (docs/adr/0075 §7).
// Its HTTPS_PROXY is the sidecar's egress router, which holds the vault
// token and sends only vault.OwnerHosts to the vault: the worker's env holds
// no token at all. Egress is open, so direct TLS needs the system roots too:
// the CA variables name bundle.pem. NODE_EXTRA_CA_CERTS adds to Node's own
// roots, so it takes the vault root alone.
func repoWorkerEnv(env []corev1.EnvVar, r RepoVault, coreHost string) []corev1.EnvVar {
	noProxy := append([]string{"127.0.0.1", "localhost"}, repoNoProxy...)
	if coreHost != "" {
		noProxy = append(noProxy, coreHost)
	}
	out := vaultWorkerEnv(env, fmt.Sprintf("http://127.0.0.1:%d", SidecarEgressPort), noProxy, vaultCADir+"/bundle.pem", "", r.GitAuthorName, r.GitAuthorEmail)
	for i := range out {
		if out[i].Name == "NODE_EXTRA_CA_CERTS" {
			out[i].Value = vaultCADir + "/ca.pem"
		}
	}
	return out
}

// vaultWorkerEnv is the env both vault profiles share. definition is
// FLEET_DEFINITION, set for a definition pod only.
func vaultWorkerEnv(env []corev1.EnvVar, proxyURL string, noProxy []string, caFile, definition, gitName, gitEmail string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+24)
	for _, e := range env {
		if e.Name != "LEASE_ID" && e.Name != "GH_TOKEN" && e.Name != "CLAUDE_CODE_OAUTH_TOKEN" {
			out = append(out, e)
		}
	}
	if u, err := url.Parse(proxyURL); err == nil && u.Hostname() != "" && !slices.Contains(noProxy, u.Hostname()) {
		noProxy = append(noProxy, u.Hostname())
	}
	np := strings.Join(noProxy, ",")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy"} {
		out = append(out, corev1.EnvVar{Name: k, Value: proxyURL})
	}
	out = append(out, corev1.EnvVar{Name: "NO_PROXY", Value: np}, corev1.EnvVar{Name: "no_proxy", Value: np}, corev1.EnvVar{Name: "NODE_USE_ENV_PROXY", Value: "1"})
	for _, k := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		out = append(out, corev1.EnvVar{Name: k, Value: caFile})
	}
	out = append(out,
		corev1.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: VaultPlaceholder},
		corev1.EnvVar{Name: "GH_TOKEN", Value: VaultPlaceholder},
		corev1.EnvVar{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
		// Telemetry and update checks: a definition vault denies them, and a
		// repo pod would send them through its vault for nothing.
		corev1.EnvVar{Name: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Value: "1"},
	)
	if definition != "" {
		out = append(out, corev1.EnvVar{Name: "FLEET_DEFINITION", Value: definition})
	}
	return append(out,
		corev1.EnvVar{Name: "GIT_AUTHOR_NAME", Value: gitName},
		corev1.EnvVar{Name: "GIT_COMMITTER_NAME", Value: gitName},
		corev1.EnvVar{Name: "GIT_AUTHOR_EMAIL", Value: gitEmail},
		corev1.EnvVar{Name: "GIT_COMMITTER_EMAIL", Value: gitEmail},
		corev1.EnvVar{Name: "GIT_CONFIG_COUNT", Value: "1"},
		corev1.EnvVar{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
		corev1.EnvVar{Name: "GIT_CONFIG_VALUE_0", Value: sessionWorkdir},
	)
}

// ResolveImages says which images a pod created with this `image` override
// will actually run — the worker container's, and ONLY the worker
// container's.
//
// The clone init container and the sidecar keep the fleet's own images.
// Cloning and diff telemetry are fleet-owned behavior that needs git and a
// specific binary at a specific path; a repo-supplied image is not required to
// have either, and a repo that set `image` to something without git would
// otherwise fail in the init container — before the agent ever starts, with an
// error about the wrong thing entirely.
//
// CreateWorkerPod calls this itself rather than the caller passing the result
// in, so what CreateWorkerPodResponse reports can never be an image the pod
// spec didn't use.
func (c *Client) ResolveImages(override string) (worker, sidecar string) {
	worker = c.WorkerImage
	if override != "" {
		worker = override
	}
	return worker, c.SidecarImage
}

func (c *Client) CreateWorkerPod(ctx context.Context, spec WorkerPodSpec) error {
	taskID, repo := spec.SessionID, spec.Repo
	leaseID, resumeSessionID, resumeFromSeq := spec.LeaseID, spec.ResumeID, spec.ResumeFromSeq
	toolKeys, serviceIngredients, extraEnv := spec.ToolKeys, spec.ServiceRefs, spec.ExtraEnv

	workerImage, _ := c.ResolveImages(spec.Image)

	name := WorkerResourceName(taskID)
	labels := WorkerLabels(taskID, repo)
	// A version that is not a valid label value (a ref with a slash) would
	// fail the whole create, so it is left off; core still gets it from
	// CreateWorkerPodResponse.fleet_version.
	if c.Version != "" && len(validation.IsValidLabelValue(c.Version)) == 0 {
		labels[VersionLabel] = c.Version
	}
	if spec.Definition != nil {
		if spec.Definition.ProxyURL == "" || spec.Definition.DeadlineSeconds <= 0 {
			return fmt.Errorf("definition pod %s: no vault token or deadline (docs/adr/0065)", taskID)
		}
		labels[DefinitionLabel] = spec.Definition.Name
	}
	if r := spec.RepoVault; r != nil {
		if spec.Definition != nil || r.ProxyURL == "" || r.DeadlineSeconds <= 0 || r.Vault == "" {
			return fmt.Errorf("repo pod %s: no vault token or deadline (docs/adr/0075)", taskID)
		}
		labels[VaultOwnerLabel] = r.Vault
	}

	// The session's own working volume, created before the Job so the Job can
	// reference it. local-path + WaitForFirstConsumer, and the pinning is the
	// feature rather than a cost (docs/adr/0048 §4): the volume's node
	// affinity forces every later warm of this session back to the node that
	// already holds its tree and its warm dependency cache, with no affinity
	// rules to write and no re-clone.
	if err := c.EnsureSessionPVC(ctx, taskID, repo); err != nil {
		return fmt.Errorf("ensure session pvc: %w", err)
	}

	sidecarRestartAlways := corev1.ContainerRestartPolicyAlways

	ingredientInitContainers, ingredientEnv, toolsVol, toolsMount, err := buildIngredients(toolKeys, serviceIngredients, ClusterAccess{ExecutorAddr: c.ExecutorAddr, AuthToken: c.ThotAuthToken})
	if err != nil {
		return fmt.Errorf("build ingredients: %w", err)
	}

	workerEnv := []corev1.EnvVar{
		{Name: "SESSION_ID", Value: taskID},
		{Name: "TARGET_REPO", Value: repo},
		{Name: "LEASE_ID", Value: leaseID},
		// 127.0.0.1, not "localhost": the sidecar binds IPv4 loopback only, and
		// "localhost" resolves to ::1 as well on a dual-stack image. Whether
		// that works then depends on the client's happy-eyeballs behaviour
		// rather than on anything in this repo.
		{Name: "SIDECAR_MCP_ADDR", Value: fmt.Sprintf("127.0.0.1:%d", SidecarMCPPort)},
		{Name: "SIDECAR_API_ADDR", Value: fmt.Sprintf("127.0.0.1:%d", SidecarAPIPort)},
		// No GH_TOKEN or CLAUDE_CODE_OAUTH_TOKEN here: every pod gets
		// placeholders the vault overwrites on the wire (docs/adr/0075).
		{Name: "CLAUDE_MODEL", Value: c.ClaudeModel},
		{Name: "MAX_TURNS", Value: c.MaxTurns},
		{Name: "WORKTREE_PATH", Value: sessionWorkdir},
		{Name: "CLAUDE_CONFIG_DIR", Value: claudeConfigDir},
		// Without this Playwright looks under $HOME/.cache/ms-playwright and
		// reports the browser as not installed — which is precisely how
		// docs/adr/0044's last failure presented, just with the browsers in a
		// different place. The mount above is inert unless this points at it.
		{Name: "PLAYWRIGHT_BROWSERS_PATH", Value: browsersDir},
		{Name: "RESUME_SESSION_ID", Value: resumeSessionID},
		{Name: "RESUME_FROM_SEQ", Value: strconv.FormatInt(resumeFromSeq, 10)},
		{Name: "LOG_LEVEL", Value: c.LogLevel},
		// The Agent SDK emits `tool_progress` (a long-running Bash call's
		// elapsed time) only when it believes it is running remotely or in
		// a container — it checks CLAUDE_CODE_REMOTE/CLAUDE_CODE_CONTAINER_ID
		// and otherwise drops the event before it reaches the message
		// stream. Every worker genuinely is a container, and without this
		// the worker's tool_progress relay is unreachable code: a
		// four-minute Bash call stays indistinguishable from a hung pod.
		// Value is only tested for emptiness by the SDK; the task ID makes
		// it identifiable in a log.
		{Name: "CLAUDE_CODE_CONTAINER_ID", Value: taskID},
	}
	coreHost, _, _ := net.SplitHostPort(c.CoreGRPCAddr)
	switch {
	case spec.Definition != nil:
		workerEnv = definitionWorkerEnv(workerEnv, *spec.Definition, coreHost)
	case spec.RepoVault != nil:
		workerEnv = repoWorkerEnv(workerEnv, *spec.RepoVault, coreHost)
	}
	if c.GitURLRewrite != "" {
		workerEnv = withGitURLRewrite(workerEnv, c.GitURLRewrite, taskID)
	}
	workerEnv = append(workerEnv, cacheEnv()...)
	workerEnv = append(workerEnv, contextBudgetEnv()...)
	workerEnv = append(workerEnv, ingredientEnv...)
	workerEnv = append(workerEnv, extraEnv...)

	// Four mounts, split by access pattern rather than by uniformity
	// (docs/adr/0048 §4). The old single whole-PVC mount put everything —
	// working tree, node_modules, SDK state — on one Longhorn RWX volume
	// measured at 10 MB/s, where a cold `bun install` could not finish in
	// three minutes. The same install takes 2.4 seconds on node-local disk.
	workerMounts := []corev1.VolumeMount{
		// Node-local, per session, durable across warms.
		{Name: "session", MountPath: "/workspace", SubPath: "tree"},
		// Dependency caches. Same volume, so same node-local speed, and warm
		// across every warm of this session. Deliberately NOT shared between
		// sessions: a per-node volume is not a shape Kubernetes offers, and
		// sharing was never where the measured win came from.
		{Name: "session", MountPath: "/cache", SubPath: "cache"},
		// The clone cache, read-only. Small and read-mostly, so the network
		// volume's latency does not matter — and read-only is a real boundary
		// for the first time: a session cannot corrupt the cache every other
		// session clones from.
		repoCacheMount(spec),
		// SDK resume state. On replicated storage because losing it loses the
		// ability to resume the conversation at all, which is the one thing
		// here that git is not already a backup of.
		{Name: "shared", MountPath: claudeConfigDir, SubPath: "claude-home/" + taskID},
		// docs/adr/0063: the rendered context packs, read-only, so the agent
		// cannot edit the rules it runs under. A sibling of claude-home/<id>.
		{Name: "shared", MountPath: genomeDir, SubPath: "claude-home/" + taskID + "-genome", ReadOnly: true},
		// Playwright's browser builds, read-only. 2 GB that every session can
		// use and no session may write — see browsersDir.
		{Name: "shared", MountPath: browsersDir, SubPath: browsersSubPath, ReadOnly: true},
	}
	if toolsMount != nil {
		workerMounts = append(workerMounts, *toolsMount)
	}

	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// fsGroup, and it is load-bearing rather than hardening.
		//
		// Kubelet creates a subPath directory root-owned 0755, and every
		// container here runs as `bun` (uid 1000, worker/Dockerfile). Without
		// this, the clone init container cannot write /workspace and no tool
		// can write /cache — on a real StorageClass. It has not bitten yet
		// only because kind's hostPath happens to be world-writable, which is
		// the same accident that hid the "dubious ownership" bug until this
		// ran against a real cluster.
		//
		// fsGroup makes kubelet chgrp the volume to this GID and set g+w, so
		// both subPaths become writable without anything running as root.
		SecurityContext: &corev1.PodSecurityContext{FSGroup: int64Ptr(1000)},
		// Sessions belong on the two worker nodes. The control-plane nodes are
		// 2 vCPU / 4 GB with ~33 GiB allocatable — too small to build anything,
		// and filling one is a cluster incident rather than a session failure.
		//
		// Empty means no constraint, which is what /kind-local relies on: its
		// single node carries no label and every pod must still schedule.
		NodeSelector: c.SessionNodeSelector,
		// sidecar runs as a native sidecar (K8s 1.29+, this cluster is
		// v1.35): an init container with RestartPolicy Always plus a
		// StartupProbe, so kubelet blocks starting the worker container
		// until the sidecar is actually accepting connections. Without
		// this, both containers start concurrently and the worker's
		// first sidecar call can lose the race (observed live: worker
		// crashed 6ms after start, ~7s before the sidecar logged
		// "listening").
		InitContainers: []corev1.Container{
			// Ordered first, and it must stay first: a plain init container
			// runs to completion before the native sidecar below it starts,
			// so the working tree exists before anything tries to read it.
			//
			// This is the only place both volumes are mounted at once, which
			// is why the clone happens here rather than in the provisioner
			// (docs/adr/0048 §5). It also means the clone runs on the node
			// that owns the volume — node-local, not across the 10 MB/s
			// fabric the provisioner would have crossed.
			cloneInitContainer(c.WorkerImage, spec),
			{
				Name:          "sidecar",
				Image:         c.SidecarImage,
				RestartPolicy: &sidecarRestartAlways,
				Env: []corev1.EnvVar{
					{Name: "SESSION_ID", Value: taskID},
					{Name: "TARGET_REPO", Value: repo},
					{Name: "MCP_PORT", Value: fmt.Sprint(SidecarMCPPort)},
					{Name: "LOCAL_API_PORT", Value: fmt.Sprint(SidecarAPIPort)},
					{Name: "HEALTH_PORT", Value: fmt.Sprint(SidecarHealthPort)},
					// The sidecar, not the worker, is what talks to core, so
					// the credential CoreService checks has to be here too —
					// the worker's own copy authenticates nothing.
					{Name: "LEASE_ID", Value: spec.LeaseID},
					{Name: "WORKTREE_PATH", Value: sessionWorkdir},
					{Name: "LOG_LEVEL", Value: c.LogLevel},
					// Without this, the sidecar falls back to its own
					// separately-hardcoded default, which only happens to
					// match prod's release-prefixed core Service name — it
					// silently doesn't resolve against kind-local's
					// unprefixed one (confirmed live: sidecar stuck at
					// /readyz 503 forever, worker never starts).
					{Name: "CORE_GRPC_ADDR", Value: c.CoreGRPCAddr},
					// FLEET_ENDPOINTS is gone with the roster it carried
					// (docs/adr/0048 §6). It told the sidecar where this
					// session's sandbox would answer, so the first run_command
					// could dial it directly rather than relaying through
					// core. There is no sandbox and no run_command; the
					// sidecar's only outbound connection is the one to core
					// it dials from CORE_GRPC_ADDR above.
					// Deliberately no THOT_* here. ADR-0035's ask_thot is
					// gone, and the executor token now belongs only to the
					// worker container of a cluster-access task (see
					// buildIngredients). Putting it back on every sidecar
					// would hand the cluster credential to tasks that have
					// no business holding it.
					//
					// Git's ownership guard (CVE-2022-24765) — the same one the
					// clone init container disables for itself. This container
					// has no $HOME to write a gitconfig into and runs as a
					// different user than the one that owns the tree, so every
					// telemetry `git diff` would refuse with "detected dubious
					// ownership". That failure surfaces only as permanently-zero
					// diff stats, never as an error anyone sees.
					{Name: "GIT_CONFIG_COUNT", Value: "1"},
					{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
					{Name: "GIT_CONFIG_VALUE_0", Value: "*"},
				},
				// mcp and local-api are loopback-bound inside the container, so
				// declaring them here buys nothing and reads as if they were
				// reachable. Only the health listener actually binds the pod IP.
				Ports: []corev1.ContainerPort{
					{Name: "health", ContainerPort: SidecarHealthPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					// The session's working tree, for the telemetry loop's
					// `git diff` — the sidecar needs to see the same files
					// the agent is editing.
					//
					// A SubPath is fine here now, where it was actively
					// wrong before: the old layout mounted the whole PVC
					// because a LINKED WORKTREE's .git is an absolute-path
					// gitlink back to repos/<repo>/.git/worktrees/<id>, so
					// scoping the mount severed it and every git command
					// answered "not a git repository". There are no linked
					// worktrees any more — this is a real clone with its
					// own .git directory (docs/adr/0048 §5).
					{Name: "session", MountPath: sessionWorkdir, SubPath: "tree"},
					// ...but a `git clone --shared` does not COPY objects:
					// .git/objects/info/alternates points back at the cache,
					// so without this mount `git diff --numstat HEAD` cannot
					// read the HEAD tree and the telemetry loop reports
					// nothing for every session. Seeing the files is not the
					// same as being able to run git on them.
					repoCacheMount(spec),
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
					// Raised from 250m/256Mi: in a repo pod every HTTPS
					// connection the worker makes is tunnelled through this
					// container (sidecar/internal/egress), and an OOM here
					// drops all of them, MCP included. Definition pods get
					// the same limit: one number, requests unchanged.
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1000m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				// HTTP on /readyz, not a bare TCP check: a TCP probe only
				// proves the sidecar process is listening, not that it can
				// actually reach core — a sidecar with zero core
				// connectivity passed a TCP probe every time, unblocking
				// the worker container against a core it couldn't talk to
				// (observed live: worker crashed 6ms after start via an
				// unguarded first sidecar call, during a core rollout
				// blip). /readyz only returns 200 once the sidecar's
				// coreclient has an actual live connection. Budget widened
				// from the prior ~30s to ~2min to ride out a core rollout,
				// not just a process-start race.
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						// SidecarHealthPort, NOT SidecarAPIPort: kubelet dials
						// this at the pod IP, and the API listener is bound to
						// 127.0.0.1. Pointing it at the API port fails every
						// probe for the full 120s budget and then kills a pod
						// whose sidecar is working fine.
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/readyz",
							Port: intstr.FromInt32(SidecarHealthPort),
						},
					},
					PeriodSeconds:    2,
					FailureThreshold: 60,
				},
			},
		},
		Containers: []corev1.Container{
			{
				Name:         "worker",
				Image:        workerImage,
				Env:          workerEnv,
				VolumeMounts: workerMounts,
				// This is the e2e sandbox's build envelope, moved here to
				// follow the builds. 250m/512Mi req and 2000m/2Gi lim were
				// the numbers from when this container ran a Claude Code
				// session and nothing else; bc5da8f then deliberately raised
				// the SANDBOX to 1000m/1Gi req and 4000m/4Gi lim, on the
				// finding that compiles, test suites and dependency installs
				// do not fit in the smaller envelope, and that the 250m
				// request was the worse half of it — a request sets the CFS
				// weight, so under any node contention the pod installed
				// dependencies with a quarter core. Six days later
				// docs/adr/0048 §6 deleted the sandbox and moved every build
				// into this container's own Bash, and the sizing did not come
				// with it. That is why the crash is new: nothing here got
				// heavier, the heavy work moved in.
				//
				// The consequence is worse than slow, because cgroup v2 sets
				// memory.oom.group on a container scope: crossing the limit
				// does not kill the one greedy process, the kernel SIGKILLs
				// EVERY task in the container at once, PID 1 included. So the
				// symptom is not a failed Bash tool call, it is a worker whose
				// logs stop mid-sentence with no error, no `session failed`
				// line, and a Job that just goes Failed. Live, 2026-08-17:
				//
				//   Memory cgroup out of memory: Killed process 271432 (node)
				//     anon-rss:1441180kB
				//   Tasks in .../cri-containerd-<worker>.scope are going to be
				//     killed due to memory.oom.group set
				//   ... Killed process 188440 (bun)    <- the worker itself
				//   ... Killed process 189018 (claude) <- the agent
				//
				// Measured RSS at that kill: vite's node 1454Mi, claude
				// 427Mi, the Playwright MCP node 149Mi, all ten chrome
				// processes 263Mi, worker bun 90Mi — ~2.4Gi against a 2Gi
				// ceiling. The browser is 17% of that, so moving it to its own
				// pod fixes nothing (a second OOM the same evening had no
				// chrome resident at all, docs/adr/0051 rejects the split on
				// its own grounds); the build is 60%, and the ceiling has to
				// clear it.
				//
				// The CPU REQUEST went back down to 100m on 2026-10-03, and
				// the memory envelope did not move: OOM kills the container,
				// CPU only slows it. 1000m reserved a core per session that
				// one measured session never came near (peak 0.05 cores, 1m
				// rate), and it fired KubeCPUOvercommit on the very first
				// session: that rule sums cluster-wide requests against
				// allocatable minus the largest node, 9.6 cores, with 9.05
				// already requested at idle. 100m lets three sessions run
				// before it fires. The cost is bc5da8f's finding — under
				// node contention a build gets a small CFS share — and
				// k8s/provisioner/image-prepull.yaml is meant to keep that
				// rare: with the worker image cached on both session nodes,
				// image locality no longer sends every new session to the
				// node that pulled it first (see that file for when this
				// does and does not spread them).
				//
				// The 4000m/4Gi limits sit at exactly limitRange.max in
				// k8s/core.yaml, inherited from when the sandbox was pinned
				// there. A container limit ABOVE max is rejected at admission
				// and the pod is never created at all — not throttled, not
				// pending, absent. Change the two together;
				// TestCreateWorkerPod_ResourcesWithinLimitRange pins them.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(workerMaxCPU),
						corev1.ResourceMemory: resource.MustParse(workerMaxMemory),
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "session",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: SessionPVCName(taskID)},
				},
			},
			{
				Name: "shared",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
				},
			},
		},
	}
	podSpec.InitContainers = append(podSpec.InitContainers, ingredientInitContainers...)
	if toolsVol != nil {
		podSpec.Volumes = append(podSpec.Volumes, *toolsVol)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(workerJobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
	if r := spec.RepoVault; r != nil {
		// docs/adr/0075 §7: the definition's lifetime cap and CA mount. Open
		// egress, the API-server token and Service env vars stay as they were.
		job.Spec.ActiveDeadlineSeconds = int64Ptr(r.DeadlineSeconds)
		job.Annotations = map[string]string{VaultLabelAnnotation: r.VaultLabel}
		mountVaultCA(&job.Spec.Template.Spec)
		// The token goes to the sidecar's egress router only.
		for i := range job.Spec.Template.Spec.InitContainers {
			if c := &job.Spec.Template.Spec.InitContainers[i]; c.Name == "sidecar" {
				c.Env = append(c.Env,
					corev1.EnvVar{Name: "FLEET_VAULT_PROXY", Value: r.ProxyURL},
					corev1.EnvVar{Name: "FLEET_VAULT_HOSTS", Value: strings.Join(vault.OwnerHosts, ",")},
					corev1.EnvVar{Name: "EGRESS_PORT", Value: fmt.Sprint(SidecarEgressPort)},
				)
			}
		}
	}
	if d := spec.Definition; d != nil {
		// docs/adr/0065: a fixed lifetime cap, and its token's TTL is longer,
		// so the pod never runs on a dead token.
		job.Spec.ActiveDeadlineSeconds = int64Ptr(d.DeadlineSeconds)
		job.Annotations = map[string]string{VaultLabelAnnotation: d.VaultLabel}
		ps := &job.Spec.Template.Spec
		// No API-server token (the namespace's default SA would mount one,
		// readable by the agent's Read tool) and no Service env vars.
		ps.AutomountServiceAccountToken = boolPtr(false)
		ps.EnableServiceLinks = boolPtr(false)
		// REFUSED from the egress lock's DNS allowlist + musl's search
		// expansion under ndots:5 stops resolution (spike 1 (b)).
		ps.DNSConfig = &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: strPtr("1")}}}
		mountVaultCA(ps)
		// docs/adr/0063 + 0067 §4: a definition pod runs settingSources
		// ["user"], under which skills load only from the user scope, never
		// from additionalDirectories. So the genome's skills are mounted,
		// read-only, over /claude-home/skills, nested inside the read-write
		// claude-home mount (the runtime orders nested mounts by path depth,
		// not by list order). The rules stay on /genome through
		// additionalDirectories.
		ps.Containers[0].VolumeMounts = append(ps.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name: "shared", MountPath: claudeConfigDir + "/skills", SubPath: "claude-home/" + spec.SessionID + "-genome/.claude/skills", ReadOnly: true,
		})
	}

	_, err = c.Core.BatchV1().Jobs(c.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		slog.Error("k8s CreateWorkerPod", "sessionId", taskID, "repo", repo, "error", err)
		return err
	}
	slog.Info("k8s CreateWorkerPod", "sessionId", taskID, "repo", repo)
	metrics.PodsCreatedTotal.WithLabelValues("worker").Inc()
	return nil
}

// mountVaultCA mounts VaultCAConfigMap on the worker. Not optional (the API
// default): the pod never starts without the CA.
func mountVaultCA(ps *corev1.PodSpec) {
	ps.Volumes = append(ps.Volumes, corev1.Volume{Name: "agent-vault-ca", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: VaultCAConfigMap}},
	}})
	ps.Containers[0].VolumeMounts = append(ps.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "agent-vault-ca", MountPath: vaultCADir, ReadOnly: true})
}

// jobForegroundDeletion cascades to the Job's own Pod synchronously —
// without it, the default background propagation can leave the Pod
// visible for a moment after DeleteWorkerJob returns.
func jobForegroundDeletion() metav1.DeleteOptions {
	policy := metav1.DeletePropagationForeground
	return metav1.DeleteOptions{PropagationPolicy: &policy}
}

func (c *Client) DeleteWorkerJob(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.BatchV1().Jobs(c.Namespace).Delete(ctx, WorkerResourceName(taskID), jobForegroundDeletion()))
	if err != nil {
		slog.Error("k8s DeleteWorkerJob", "sessionId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteWorkerJob", "sessionId", taskID)
	metrics.PodsDeletedTotal.WithLabelValues("worker").Inc()
	return nil
}

// GetWorkerJob reads a session's worker Job from its own labels and
// annotations, the only record the provisioner has (it holds no DB
// credentials, docs/adr/0020 point 1). TearDownSession uses it for existence
// and the vault token to revoke (docs/adr/0065).
func (c *Client) GetWorkerJob(ctx context.Context, taskID string) (job LiveWorkerJob, exists bool, err error) {
	j, err := c.Core.BatchV1().Jobs(c.Namespace).Get(ctx, WorkerResourceName(taskID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return LiveWorkerJob{}, false, nil
	}
	if err != nil {
		slog.Error("k8s GetWorkerJob", "sessionId", taskID, "error", err)
		return LiveWorkerJob{}, false, err
	}
	return liveJob(j), true, nil
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ignoreAlreadyExists makes the e2e session's non-pod resources
// create-if-absent. All three (Service, Middleware, IngressRoute) are
// deterministic from the task ID alone, so an existing one is already the
// one we would have created. Needed because a pod can go away on its own —
// eviction, node drain, OOM — leaving its siblings behind; without this,
// the next request_e2e_env dies on "services \"e2e-<id>\" already exists"
// after having successfully recreated the pod, leaving a half-built
// session. Caught by TestCreateE2ESession_TerminatingPodIsNotAnExistingSession.
//
// ponytail: no reconcile-to-match. If a provisioner upgrade ever changes a
// port or a route, a session created before it keeps the old shape until
// it's torn down — switch to a server-side apply if that stops being fine.
func ignoreAlreadyExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// EgressLockName is k8s/provisioner/definition-egress.yaml's CNP.
const EgressLockName = "definition-egress-lock"

// EgressLockPresent refuses a definition warm while the egress lock is absent
// (a failed sync, a delete before selfHeal, a cluster without Cilium): the
// pod would otherwise get its token with open egress. Presence only: that
// Cilium enforces it was verified live (spike 1), not here.
func (c *Client) EgressLockPresent(ctx context.Context) error {
	if _, err := c.Dynamic.Resource(CNPGVR).Namespace(c.Namespace).Get(ctx, EgressLockName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("egress lock %s/%s: %w", c.Namespace, EgressLockName, err)
	}
	return nil
}

// systemRoots are where a CA bundle lives: debian's ca-certificates (the
// provisioner image), then macOS (a laptop's go test). The first is the base
// of bundle.pem.
var systemRoots = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem"}

func readSystemRoots() ([]byte, error) {
	var errs []error
	for _, p := range systemRoots {
		b, err := os.ReadFile(p)
		if err == nil {
			return b, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// EnsureVaultCA upserts the vault's MITM root (ca.pem), and the system roots
// plus that root (bundle.pem), into VaultCAConfigMap, writing only when they
// changed (docs/adr/0065 §4). Concurrent warms race the create
// (AlreadyExists) or the update (Conflict): the loser re-reads and compares.
func (c *Client) EnsureVaultCA(ctx context.Context, pem []byte) error {
	roots, err := readSystemRoots()
	if err != nil {
		return fmt.Errorf("system roots for the vault CA bundle: %w", err)
	}
	data := map[string]string{"ca.pem": string(pem), "bundle.pem": string(roots) + "\n" + string(pem)}
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err)
	}, func() error { return c.ensureVaultCA(ctx, data) })
}

func (c *Client) ensureVaultCA(ctx context.Context, data map[string]string) error {
	cms := c.Core.CoreV1().ConfigMaps(c.Namespace)
	cm, err := cms.Get(ctx, VaultCAConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: VaultCAConfigMap, Namespace: c.Namespace, Labels: map[string]string{"app.kubernetes.io/part-of": "agent-fleet"}},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if maps.Equal(cm.Data, data) {
		return nil
	}
	cm.Data = data
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
