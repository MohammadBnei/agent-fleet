package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeVaults struct {
	resp *agentfleetv1.ReconcileVaultsResponse
	err  error
}

func (f fakeVaults) ReconcileVaults(context.Context, []*agentfleetv1.VaultSpec) (*agentfleetv1.ReconcileVaultsResponse, error) {
	return f.resp, f.err
}

// docs/adr/0065 §2: vault errors are logged when they change, never every tick,
// and an older provisioner is not an error.
func TestReconcileVaults_LogsOnChange(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	failing := fakeVaults{resp: &agentfleetv1.ReconcileVaultsResponse{
		Results:    []*agentfleetv1.VaultResult{{Name: "dev-notes", Error: "secret GH_DEV_NOTES not set"}, {Name: "ok-one"}},
		PruneError: "list vaults: 503",
	}}
	state := ""
	for _, step := range []struct {
		name      string
		p         fakeVaults
		wantState string
		wantLog   string // "" = no line
	}{
		{"older provisioner", fakeVaults{err: status.Error(codes.Unimplemented, "x")}, "", ""},
		{"vault + prune errors", failing, "dev-notes: secret GH_DEV_NOTES not set; prune: list vaults: 503", "level=ERROR"},
		{"same errors again", failing, "dev-notes: secret GH_DEV_NOTES not set; prune: list vaults: 503", ""},
		{"older provisioner mid-rollout", fakeVaults{err: status.Error(codes.Unimplemented, "x")}, "dev-notes: secret GH_DEV_NOTES not set; prune: list vaults: 503", ""},
		{"rpc error", fakeVaults{err: errors.New("ReconcileVaults: deadline exceeded")}, "ReconcileVaults: deadline exceeded", "level=ERROR"},
		{"cleared", fakeVaults{resp: &agentfleetv1.ReconcileVaultsResponse{}}, "", "level=INFO"},
	} {
		logs.Reset()
		state = reconcileVaults(context.Background(), step.p, nil, state)
		if state != step.wantState {
			t.Errorf("%s: state %q, want %q", step.name, state, step.wantState)
		}
		got := strings.TrimSpace(logs.String())
		if step.wantLog == "" && got != "" || step.wantLog != "" && (strings.Count(got, "\n") != 0 || !strings.Contains(got, step.wantLog)) {
			t.Errorf("%s: log %q, want one %q line or none", step.name, got, step.wantLog)
		}
	}
}
