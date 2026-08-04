// Package telemetry is the sidecar's independent git-diff/branch/timing
// push (docs/adr/0020 point 5's second bullet, docs/adr/0021's "Manual
// override" discussion) — entirely decoupled from what the agent's tool
// calls do. This is the mechanism that replaced the direct-Postgres-write
// idea floated earlier in the design session: ground-truth `git diff
// --numstat` output, not a reconstruction from captured Edit/Write tool
// inputs, so it also catches file changes made via Bash (sed, mv, a
// script) that tool-input reconstruction would miss.
package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type fileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

type summary struct {
	Branch string       `json:"branch"`
	Files  []fileChange `json:"files"`
}

// Run polls worktreePath every interval and pushes a diff-stat snapshot —
// a naive line-count summary (git's own --numstat), good enough for a UI
// stat, not a substitute for reviewing the actual diff.
func Run(ctx context.Context, core *coreclient.Client, worktreePath string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		push(ctx, core, worktreePath)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func push(ctx context.Context, core *coreclient.Client, worktreePath string) {
	s, err := computeSummary(worktreePath)
	if err != nil {
		slog.Warn("telemetry: compute diff summary failed", "error", err)
		return
	}
	if len(s.Files) == 0 {
		return // nothing changed since the worktree was created — no point pushing an empty snapshot every tick
	}
	body, err := json.Marshal(s)
	if err != nil {
		slog.Warn("telemetry: marshal summary failed", "error", err)
		return
	}
	if err := core.PushToolTelemetry(ctx, string(body)); err != nil {
		slog.Warn("telemetry: push failed", "error", err)
	}
}

func computeSummary(worktreePath string) (summary, error) {
	branch, err := gitOutput(worktreePath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return summary{}, err
	}
	numstat, err := gitOutput(worktreePath, "diff", "--numstat", "HEAD")
	if err != nil {
		return summary{}, err
	}

	var files []fileChange
	for _, line := range strings.Split(numstat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0]) // "-" for binary files parses to 0 — fine for a UI stat
		removed, _ := strconv.Atoi(parts[1])
		files = append(files, fileChange{Path: parts[2], Added: added, Removed: removed})
	}
	return summary{Branch: branch, Files: files}, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
