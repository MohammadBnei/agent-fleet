package mcpserver

import (
	"bytes"
	"context"
	"fmt"
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
	// Run executes kubectl; nil runs the real one with the mounted fleet-read token.
	Run func(ctx context.Context, args []string) (string, error)
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
	"--api-group": true, "--namespaced": true, "--verbs": true, "--cached": false,
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
			val = args[i]
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
// the sidecar's own (lease, vault proxy) never reaches the child, and the pod has no Service env, so the API server
// address is named for the in-cluster config.
func runKubectl(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "KUBECACHEDIR=/tmp/kube-cache",
		"KUBERNETES_SERVICE_HOST=kubernetes.default.svc", "KUBERNETES_SERVICE_PORT=443"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	text := out.String() + errb.String()
	if len(text) > kubeOutputCap {
		text = text[:kubeOutputCap] + "\n... (truncated)"
	}
	return text, err
}

func kubectlReadHandler(k *KubeRead) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetStringSlice("args", nil)
		if refusal := validateKubeRead(args, k.Namespaces); refusal != "" {
			return mcp.NewToolResultError("kubectl_read refused: " + refusal), nil
		}
		run := k.Run
		if run == nil {
			run = runKubectl
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
