package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// KubeRead is the kubectl_read tool's grant (ADR-0084 §6, ADR-0086 §2): the definition's cluster.read, held by a
// session whose trust set holds B. Namespaces are where pod logs may be read.
type KubeRead struct {
	Namespaces []string
	// APIHost and APIPort are the API server's ClusterIP and port, from the pod's own KUBERNETES_SERVICE_HOST/PORT
	// (via config). Empty host refuses every call.
	APIHost, APIPort string
	// Core is asked before every call (ADR-0086 §2); a nil Core or any error refuses, so exposure in the tool list
	// is a convenience and never the check.
	Core KubeAuthorizer
	// Run executes kubectl; nil runs the real one with the mounted fleet-read token.
	Run func(ctx context.Context, args []string) (string, error)
}

// KubeAuthorizer is *coreclient.Client's slice: core re-reads the stored set and the definition's grant.
type KubeAuthorizer interface {
	AuthorizeKubeRead(ctx context.Context) error
}

// kubeVerbs are the only subcommands. Narrower than the executor's ValidateReadOnly list on purpose: no explain,
// diff, version or cluster-info, which the agent has no use for here.
var kubeVerbs = []string{"get", "describe", "logs", "events", "top", "api-resources"}

// kubeFlags is an allowlist, not the executor's denylist: a flag not named here is refused, so --as, --server,
// --token, --kubeconfig, -f (reads a local file), --raw and --profile-output never need naming. true = takes a value.
// Every kubectl flag that redirects who or what we talk to, or touches a file, is absent.
var kubeFlags = map[string]bool{
	"-n": true, "--namespace": true, "-A": false, "--all-namespaces": false,
	"-o": true, "--output": true, "-l": true, "--selector": true, "--field-selector": true,
	"-c": true, "--container": true, "--tail": true, "--since": true, "--since-time": true, "--limit-bytes": true,
	"--previous": false, "-p": false, "--timestamps": false, "--prefix": false, "--all-containers": false,
	"--no-headers": false, "--show-labels": false, "--show-kind": false, "--sort-by": true, "--containers": false,
	"--ignore-not-found": false, "--for": true, "--types": true, "--chunk-size": true, "--max-log-requests": true,
	"--api-group": true, "--namespaced": false, "--verbs": true, "--cached": false,
}

// validateKubeRead returns "" for an acceptable read, else why not. logNamespaces bounds `logs`: it must name one
// of them with -n (pods/log is granted only there; -A is never). Positionals (resources, names) are free.
func validateKubeRead(args []string, logNamespaces []string) string {
	if len(args) == 0 || !slices.Contains(kubeVerbs, args[0]) {
		return fmt.Sprintf("the first argument must be one of: %s", strings.Join(kubeVerbs, ", "))
	}
	var ns string
	var all bool
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, glued := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") && len(a) > 2 && !glued { // -nfoo, -ojson
			name, val, glued = a[:2], a[2:], true
		}
		takes, ok := kubeFlags[name]
		if !ok {
			return fmt.Sprintf("flag %q is not permitted", name)
		}
		if !takes && glued && strings.HasPrefix(a, "--") {
			return fmt.Sprintf("flag %q takes no value", name)
		}
		if !takes && glued { // -Afoo: a bundle of short flags; none is worth supporting
			return fmt.Sprintf("flag %q is not permitted", a)
		}
		if takes && !glued {
			if i++; i >= len(args) {
				return fmt.Sprintf("flag %q needs a value", name)
			}
			// A value never starts with "-" here, so a flag whose arity is wrong in kubeFlags cannot swallow the
			// next flag unchecked; a value that does (--since=-1h) is written glued.
			if val = args[i]; strings.HasPrefix(val, "-") {
				return fmt.Sprintf("flag %q needs a value; write a value starting with - as %s=VALUE", name, name)
			}
		}
		switch name {
		case "-n", "--namespace":
			ns = val
		case "-A", "--all-namespaces":
			all = true
		case "-o", "--output":
			// go-template-file and custom-columns-file read a file from the sidecar's own filesystem.
			if strings.Contains(val, "file") {
				return fmt.Sprintf("output %q is not permitted", val)
			}
		}
	}
	if args[0] == "logs" && (all || !slices.Contains(logNamespaces, ns)) {
		return fmt.Sprintf("logs need -n with one of: %s", strings.Join(logNamespaces, ", "))
	}
	return ""
}

const kubeOutputCap = 100_000 // ADR-0046: cap every unbounded tool result at the source.

// runKubectl runs the real kubectl as the mounted ServiceAccount. The environment is built here, not inherited:
// the sidecar's own (lease, vault proxy) never reaches the child. The API server address is the one the kubelet
// injected into this container (KUBERNETES_SERVICE_HOST/PORT, a ClusterIP present even with Service links off), not
// the name kubernetes.default.svc: the definition egress lock's DNS allowlist refuses that lookup (#434).
func runKubectl(ctx context.Context, host, port string, args []string) (string, error) {
	env, err := kubectlEnv(host, port)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectlBin, args...)
	cmd.Env = env
	out := &cappedBuffer{limit: kubeOutputCap}
	cmd.Stdout, cmd.Stderr = out, out // one writer: exec shares it, so output interleaves in order
	err = cmd.Run()
	return out.String(), err
}

// kubectlBin is a variable so a test can put a fake in its place.
var kubectlBin = "kubectl"

// kubectlEnv is the child's whole environment. It fails closed when the pod has no KUBERNETES_SERVICE_HOST or _PORT: no
// fallback to a DNS name. TLS stays verified against the mounted CA, so the apiserver cert must carry the ClusterIP
// as an IP SAN (kubeadm and kubespray add it).
func kubectlEnv(host, port string) ([]string, error) {
	if host == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set in this pod; refusing to guess the API server address")
	}
	if port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_PORT is not set in this pod; refusing to guess the API server port")
	}
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "KUBECACHEDIR=/tmp/kube-cache",
		"KUBERNETES_SERVICE_HOST=" + host, "KUBERNETES_SERVICE_PORT=" + port}, nil
}

// cappedBuffer keeps the first limit bytes and drops the rest as it goes, so a huge `logs` never sits in memory
// (ADR-0046: cap at the source).
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.truncated = true
	} else {
		c.buf.Write(p)
	}
	return len(p), nil // never an error: kubectl must keep running to exit, not die on a broken pipe
}

func (c *cappedBuffer) String() string {
	if !c.truncated {
		return c.buf.String()
	}
	return strings.ToValidUTF8(c.buf.String(), "") + "\n... (truncated)"
}

func kubectlReadHandler(k *KubeRead) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetStringSlice("args", nil)
		if refusal := validateKubeRead(args, k.Namespaces); refusal != "" {
			return mcp.NewToolResultError("kubectl_read refused: " + refusal), nil
		}
		// ADR-0086 §2: core is asked first, every call, and any failure refuses.
		if k.Core == nil {
			return mcp.NewToolResultError("kubectl_read refused: no core authorizer"), nil
		}
		if err := k.Core.AuthorizeKubeRead(ctx); err != nil {
			slog.Error("mcp kubectl_read refused by core", "error", err)
			return mcp.NewToolResultError("kubectl_read refused: " + err.Error()), nil
		}
		slog.Info("mcp kubectl_read", "verb", args[0])
		run := k.Run
		if run == nil {
			run = func(ctx context.Context, args []string) (string, error) {
				return runKubectl(ctx, k.APIHost, k.APIPort, args)
			}
		}
		out, err := run(ctx, args)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kubectl %s failed: %v\n%s", args[0], err, out)), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}

func addKubeRead(s *server.MCPServer, k *KubeRead) {
	s.AddTool(mcp.NewTool("kubectl_read",
		mcp.WithDescription("Read the cluster with kubectl, read-only, as the fleet-read identity. args is the kubectl argv without the program name; the first is the verb: "+strings.Join(kubeVerbs, ", ")+". Only a short list of read flags (-n, -o, -l, --tail, ...) is accepted. `logs` needs -n with one of: "+strings.Join(k.Namespaces, ", ")+". Secrets and configmaps are not readable."),
		mcp.WithArray("args", mcp.Required(), mcp.WithStringItems(), mcp.Description(`e.g. ["get","pods","-n","agent-fleet"]`)),
	), kubectlReadHandler(k))
}
