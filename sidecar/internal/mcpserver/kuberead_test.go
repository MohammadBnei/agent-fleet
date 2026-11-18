package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

var logNS = []string{"agent-fleet", "monitoring"}

func TestValidateKubeRead_Accepts(t *testing.T) {
	for _, args := range [][]string{
		{"get", "pods", "-n", "agent-fleet", "-o", "wide"},
		{"get", "pods", "-A", "-l", "app=core", "--no-headers"},
		{"describe", "deploy/core", "--namespace=agent-fleet"},
		{"logs", "deploy/core", "-n", "monitoring", "--tail", "100", "--previous"},
		{"logs", "core-0", "-nagent-fleet", "-c", "core"},
		{"events", "-n", "ukubi-game"},
		{"top", "nodes"},
		{"api-resources", "--verbs=list", "--namespaced"},
	} {
		if r := validateKubeRead(args, logNS); r != "" {
			t.Errorf("%v refused: %s", args, r)
		}
	}
}

func TestValidateKubeRead_Refuses(t *testing.T) {
	for name, args := range map[string][]string{
		"empty":            nil,
		"delete":           {"delete", "pod", "x"},
		"patch":            {"patch", "deploy/core", "-p", "{}"},
		"exec":             {"exec", "-it", "core", "--", "sh"},
		"rollout":          {"rollout", "restart", "deploy/core"},
		"verb not first":   {"--namespace", "x", "get", "pods"},
		"chained":          {"get pods; kubectl delete pod x"},
		"as":               {"get", "pods", "--as", "system:admin"},
		"as glued":         {"get", "pods", "--as=system:admin"},
		"server":           {"get", "pods", "--server=https://evil"},
		"short server":     {"get", "pods", "-shttps://evil"},
		"token":            {"get", "pods", "--token=abc"},
		"kubeconfig":       {"get", "pods", "--kubeconfig", "/tmp/x"},
		"filename":         {"get", "-f", "/run/secrets/lease"},
		"raw":              {"get", "--raw", "/api/v1/secrets"},
		"template file":    {"get", "pods", "-o", "go-template-file=/etc/passwd"},
		"template file -o": {"get", "pods", "-ogo-template-file=/etc/passwd"},
		"watch":            {"get", "pods", "-w"},
		"profile output":   {"get", "pods", "--profile-output=/tmp/x"},
		"end of options":   {"get", "--", "--as=x"},
		"bool with value":  {"get", "pods", "--no-headers=false"},
		"missing value":    {"get", "pods", "-n"},
		"logs no ns":       {"logs", "core-0"},
		"logs all ns":      {"logs", "core-0", "-A"},
		"logs other ns":    {"logs", "core-0", "-n", "kube-system"},
		"logs ns prefix":   {"logs", "core-0", "-n", "monitoring-x"},
		// A flag whose arity is wrong in kubeFlags must never swallow the next flag unchecked.
		"bool then as":      {"api-resources", "--namespaced", "--as=x"},
		"bool then token":   {"api-resources", "--namespaced", "--token=X", "--namespaced", "--server=https://h"},
		"value then flag":   {"get", "pods", "-n", "--as=x"},
		"selector then as":  {"get", "pods", "-l", "--as=x"},
		"tail then profile": {"logs", "x", "-n", "agent-fleet", "--tail", "--profile-output=/tmp/x"},
	} {
		if validateKubeRead(args, logNS) == "" {
			t.Errorf("%s: %v must be refused", name, args)
		}
	}
	if validateKubeRead([]string{"logs", "x", "-n", "agent-fleet"}, nil) == "" {
		t.Error("logs with no granted namespaces must be refused")
	}
}

// Every value-taking flag followed by a forbidden flag is refused, whatever kubeFlags says about its arity.
func TestValidateKubeRead_ValueFlagNeverSwallowsNext(t *testing.T) {
	for name, takes := range kubeFlags {
		if !takes {
			continue
		}
		if validateKubeRead([]string{"get", "pods", name, "--as=x"}, logNS) == "" {
			t.Errorf("%s swallowed a following --as", name)
		}
	}
}

type fakeCore struct {
	err   error
	calls int
}

func (f *fakeCore) AuthorizeKubeRead(context.Context) error { f.calls++; return f.err }

func TestKubectlReadHandler(t *testing.T) {
	var ran []string
	k := &KubeRead{Namespaces: logNS, Core: &fakeCore{}, Run: func(_ context.Context, a []string) (string, error) { ran = a; return "ok", nil }}
	call := func(args ...any) *mcp.CallToolResult {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"args": args}
		res, err := kubectlReadHandler(k)(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("get", "pods"); res.IsError || !slices.Equal(ran, []string{"get", "pods"}) {
		t.Errorf("read not run: %v %v", res, ran)
	}
	ran = nil
	if res := call("delete", "pod", "x"); !res.IsError || ran != nil {
		t.Errorf("a refused call must not run kubectl: %v %v", res, ran)
	}
}

// ADR-0086 §2: core is asked first and the tool fails closed: a refusal, an error or no authorizer never runs kubectl.
func TestKubectlReadHandler_AsksCoreFirst(t *testing.T) {
	for name, core := range map[string]KubeAuthorizer{
		"refused": &fakeCore{err: errors.New("agent.rule_of_two")},
		"nil":     nil,
	} {
		ran := false
		k := &KubeRead{Namespaces: logNS, Core: core, Run: func(context.Context, []string) (string, error) { ran = true; return "ok", nil }}
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"args": []any{"get", "pods"}}
		res, err := kubectlReadHandler(k)(context.Background(), req)
		if err != nil || !res.IsError || ran {
			t.Errorf("%s: res %v err %v ran %v, want an error result and no kubectl", name, res, err, ran)
		}
	}
	fc := &fakeCore{}
	k := &KubeRead{Namespaces: logNS, Core: fc, Run: func(context.Context, []string) (string, error) { return "ok", nil }}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"args": []any{"get", "pods"}}
	if res, _ := kubectlReadHandler(k)(context.Background(), req); res.IsError || fc.calls != 1 {
		t.Errorf("an authorized call: %v, core asked %d times", res, fc.calls)
	}
}

// A fake kubectl on disk: the child's environment is exactly the built list (nothing of the sidecar's own), and its
// output is capped at the source.
func TestRunKubectl_EnvAndCap(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "kubectl")
	script := "#!/bin/sh\nif [ \"$1\" = big ]; then head -c 300000 /dev/zero | tr '\\0' 'é'; exit 0; fi\nenv\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := kubectlBin
	kubectlBin = fake
	t.Cleanup(func() { kubectlBin = old })
	t.Setenv("LEASE_ID", "sidecar-lease-secret")
	t.Setenv("FLEET_VAULT_PROXY", "http://vault-secret")

	out, err := runKubectl(context.Background(), "10.233.0.1", "443", []string{"get", "pods"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("the sidecar's environment reached kubectl:\n%s", out)
	}
	want, _ := kubectlEnv("10.233.0.1", "443")
	for _, e := range want {
		if !strings.Contains(out, e+"\n") {
			t.Errorf("child lacks %s:\n%s", e, out)
		}
	}
	if strings.Contains(out, "kubernetes.default.svc") {
		t.Errorf("child was pointed at the DNS name the egress lock refuses:\n%s", out)
	}

	big, err := runKubectl(context.Background(), "10.233.0.1", "443", []string{"big"})
	if err != nil {
		t.Fatal(err)
	}
	if len(big) > kubeOutputCap+len("\n... (truncated)") || !strings.HasSuffix(big, "(truncated)") || !utf8.ValidString(big) {
		t.Errorf("output not capped on a rune boundary: %d bytes, valid %v", len(big), utf8.ValidString(big))
	}
}

func TestKubectlReadOnlyWhenGranted(t *testing.T) {
	has := func(k *KubeRead) bool {
		_, ok := newMCPServer(nil, nil, k).ListTools()["kubectl_read"]
		return ok
	}
	if has(nil) {
		t.Error("kubectl_read served without the grant")
	}
	if !has(&KubeRead{Namespaces: logNS}) {
		t.Error("kubectl_read missing with the grant")
	}
}

// #434: the API server address is the pod's own injected ClusterIP, never kubernetes.default.svc.
func TestKubectlEnv_FromPodServiceEnv(t *testing.T) {
	env, err := kubectlEnv("10.233.0.1", "6443")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"KUBERNETES_SERVICE_HOST=10.233.0.1", "KUBERNETES_SERVICE_PORT=6443"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env lacks %s: %v", want, env)
		}
	}
	if strings.Contains(joined, "kubernetes.default.svc") {
		t.Errorf("env names the refused DNS name: %v", env)
	}
}

func TestKubectlEnv_MissingHostFailsClosed(t *testing.T) {
	if _, err := kubectlEnv("", "443"); err == nil {
		t.Fatal("no KUBERNETES_SERVICE_HOST must be an error")
	}
	if _, err := kubectlEnv("10.233.0.1", ""); err == nil {
		t.Fatal("no KUBERNETES_SERVICE_PORT must be an error")
	}
	old := kubectlBin
	kubectlBin = filepath.Join(t.TempDir(), "must-not-run")
	t.Cleanup(func() { kubectlBin = old })
	out, err := runKubectl(context.Background(), "", "443", []string{"get", "pods"})
	if err == nil || out != "" {
		t.Errorf("runKubectl must refuse without running kubectl: out %q err %v", out, err)
	}
}

// The real path (Run nil) uses the KubeRead address fields and refuses when the host is empty.
func TestKubectlReadHandler_NoAPIHostRefuses(t *testing.T) {
	old := kubectlBin
	kubectlBin = filepath.Join(t.TempDir(), "must-not-run")
	t.Cleanup(func() { kubectlBin = old })
	k := &KubeRead{Namespaces: logNS, Core: &fakeCore{}}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"args": []any{"get", "pods"}}
	res, err := kubectlReadHandler(k)(context.Background(), req)
	if err != nil || !res.IsError || !strings.Contains(fmt.Sprint(res.Content), "KUBERNETES_SERVICE_HOST") {
		t.Errorf("want a refusal naming KUBERNETES_SERVICE_HOST, got %+v, %v", res, err)
	}
}
