// Package catalog is the bounded environment-recipe ingredient catalog
// (docs/adr/0034) — a repo's profile picks from these six known keys,
// never an arbitrary container. Adding a new key is a deliberate code
// change here, never config-only.
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

var Tools = map[string]ToolDef{
	"go-toolchain": {
		CopyImage: "golang:1.26-alpine",
		CopyCmd:   []string{"sh", "-c", "cp -r /usr/local/go /opt/tools/go"},
	},
	"bun-toolchain": {
		CopyImage: "oven/bun:1",
		CopyCmd:   []string{"sh", "-c", "mkdir -p /opt/tools/bin && cp /usr/local/bin/bun /opt/tools/bin/bun"},
	},
	"golangci-lint": {
		CopyImage: "golangci/golangci-lint:latest",
		CopyCmd:   []string{"sh", "-c", "mkdir -p /opt/tools/bin && cp /usr/bin/golangci-lint /opt/tools/bin/golangci-lint"},
	},
	"buf": {
		CopyImage: "bufbuild/buf:latest",
		CopyCmd:   []string{"sh", "-c", "mkdir -p /opt/tools/bin && cp /usr/local/bin/buf /opt/tools/bin/buf"},
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

// KnownToolKey and KnownServiceKey are the fail-loud membership checks used
// before any Kubernetes object is built — an unknown key from a stale or
// hand-edited repo_profiles row is caught here, not deep inside pod
// construction.
func KnownToolKey(key string) bool {
	_, ok := Tools[key]
	return ok
}

func KnownServiceKey(key string) bool {
	_, ok := Services[key]
	return ok
}
