// Package catalog is what is left of the bounded environment-recipe
// ingredient catalog (docs/adr/0034, superseded by docs/adr/0048): one tool
// key and two service kinds.
//
// It is no longer where a repo's toolchain is declared — that is
// `repos.image`, dashboard-editable and needing no release (docs/adr/0048
// §6). What stays here is what genuinely cannot be expressed as "which image
// does this repo run": a privilege grant, and two services that need cluster
// RBAC to provision.
package catalog

// ToolDef is a pod-local ingredient: a set of binaries copied onto a
// shared emptyDir by a terminating init container, not a running process.
// The agent's own Bash tool needs these as real local binaries in its own
// filesystem — a sibling container's process/filesystem namespace isn't
// reachable from there.
type ToolDef struct {
	// CopyImage already contains the binary — the init container's only
	// job is to copy it onto the shared /opt/tools volume.
	CopyImage string
	// CopyCmd stages the binary(ies) into /opt/tools inside CopyImage's own
	// filesystem. Binary paths below are best-effort per each project's
	// documented image layout — verify with
	// `docker run --rm <image> which <bin>` before relying on them; a
	// wrong path fails loud (RestartPolicy: Never means the whole pod
	// fails immediately), it just fails at pod-creation time, not here.
	CopyCmd []string
}

// The four toolchain entries (go-toolchain, bun-toolchain, golangci-lint,
// buf) lived here until docs/adr/0048 §6 replaced them with `repos.image` —
// "an init container copying a Go toolchain onto an emptyDir is an elaborate
// way of saying this repo needs Go". They had already lost their producer at
// that point: ToolKeysFor returns only cluster-access.
//
// They are deleted rather than left harmless, because go-toolchain put
// /opt/tools/go/bin FIRST on PATH — so any profile still naming it silently
// shadowed the Go the worker image bakes, with a different patch version and
// no error anywhere.
var Tools = map[string]ToolDef{
	// cluster-access is the odd one out: it stages a kubectl *shim* that
	// forwards argv to thot-executor, the only process in the fleet
	// holding cluster RBAC (docs/adr/0037).
	//
	// This is what lets a thot session be an ordinary worker pod — the pod
	// itself has no Kubernetes credentials, so `kubectl` here is just an
	// RPC client. The agent writes normal bash and canUseTool gates it
	// like any other Bash call.
	//
	// Copied from the executor's own image so the shim and the service it
	// talks to are always built from the same commit; a drifting pair
	// would fail at runtime with a confusing proto error.
	"cluster-access": {
		// Floating :latest on purpose — see above. Zot's retention policy pins
		// the `latest` tag explicitly for exactly this reason
		// (infra-bootstrap gitops/platform/values/zot/values.yaml): a 5-tag
		// retention that let `latest` age out would break every
		// cluster-access session with no version bump to point at.
		CopyImage: "registry.bnei.lan:5000/agent-fleet-executor:latest",
		CopyCmd:   []string{"sh", "-c", "mkdir -p /opt/tools/bin && cp /usr/local/bin/kubectl-shim /opt/tools/bin/kubectl"},
	},
}

// ServiceDef is a service-kind ingredient (postgres/redis) — see
// docs/adr/0034 for the three scope modes (pod-scoped/task-scoped/
// repo-scoped) that control how it's materialized. Image is deliberately
// not here: unlike a tool's CopyImage (tied 1:1 to specific in-image binary
// paths, a code-level fact), a service's image tag is an operational
// concern (version bumps for security patches) resolved from the
// provisioner's own config (POSTGRES_IMAGE/REDIS_IMAGE) at the call site.
type ServiceDef struct {
	Port int32
	// AdminUser is "" for redis (no user/auth concept in the default image).
	AdminUser string
	// NeedsPVC is true for postgres (durable data directory) and false for
	// redis (pure cache, an emptyDir is enough and one less PVC to manage).
	NeedsPVC bool
	// EnvVarName is the connection-URL env var the consuming app actually
	// looks for — the de facto standard name for that specific technology
	// (Prisma/most ORMs read DATABASE_URL, ioredis/most Redis clients read
	// REDIS_URL), not a made-up SERVICE_<KEY>_URL scheme. Confirmed the
	// hard way via /kind-local: dream-analyst's own Prisma config errored
	// with "Cannot resolve environment variable: DATABASE_URL" until this
	// matched what the app itself expects.
	EnvVarName string
}

var Services = map[string]ServiceDef{
	"postgres": {Port: 5432, AdminUser: "postgres", NeedsPVC: true, EnvVarName: "DATABASE_URL"},
	"redis":    {Port: 6379, NeedsPVC: false, EnvVarName: "REDIS_URL"},
}

// Membership is checked at the point of use with a plain `def, ok := Tools[key]`
// / `Services[key]` — an unknown key from a stale or hand-edited repo_profiles
// row is caught before any Kubernetes object is built, not deep inside pod
// construction. See k8s/ingredients.go and grpcserver/server.go.
