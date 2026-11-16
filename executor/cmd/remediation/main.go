// Command remediation is the remediation executor (ADR-0086 §3): it pulls one
// approved remediation row by id from core and runs only catalogue actions,
// under its own ServiceAccount. thot-executor is read-only; this is the only
// agent-facing process that mutates.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/MohammadBnei/agent-fleet/executor/internal/config"
	"github.com/MohammadBnei/agent-fleet/executor/internal/kubectl"
	"github.com/MohammadBnei/agent-fleet/executor/internal/remediation"
)

// errNoCore stands in for core's remediations endpoint, which is S05's.
var errNoCore = errors.New("core remediations API is not available yet")

// ponytail: placeholder Source until S05 lands the endpoint; swap it for the real client.
type noCore struct{}

func (noCore) Pull(context.Context, string) (remediation.Row, error) {
	return remediation.Row{}, errNoCore
}
func (noCore) Report(context.Context, string, remediation.Outcome) error { return errNoCore }

func main() {
	id := config.RemediationFromProcess().ID
	if id == "" {
		slog.Error("REMEDIATION_ID is unset")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := remediation.Run(ctx, noCore{}, kubectl.Run, id); err != nil {
		slog.Error("remediation failed", "id", id, "error", err)
		os.Exit(1)
	}
}
