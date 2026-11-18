package mcpserver

import (
	"context"
	"slices"
	"testing"

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
		{"api-resources", "--verbs=list"},
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
	} {
		if validateKubeRead(args, logNS) == "" {
			t.Errorf("%s: %v must be refused", name, args)
		}
	}
	if validateKubeRead([]string{"logs", "x", "-n", "agent-fleet"}, nil) == "" {
		t.Error("logs with no granted namespaces must be refused")
	}
}

func TestKubectlReadHandler(t *testing.T) {
	var ran []string
	k := &KubeRead{Namespaces: logNS, Run: func(_ context.Context, a []string) (string, error) { ran = a; return "ok", nil }}
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
