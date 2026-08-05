// Package sweep is the automated half of worktree/branch cleanup
// (reliability-findings.md #2) — periodically deletes agent/<taskId>
// branches (and their worktrees) whose upstream tracking branch is gone,
// on the same ticker-poll shape every other loop in this codebase uses.
// The manual half (a human explicitly deleting via the dashboard) lives
// in grpcserver's ListWorktrees/DeleteWorktree RPCs and is the fallback
// for the case this sweep can't reach — a branch abandoned before its
// first push, which never gets an upstream ref to compare against (see
// git.Manager.SweepGoneBranches's own doc comment).
package sweep

import (
	"context"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
)

type Loop struct {
	git *git.Manager
}

func New(gitMgr *git.Manager) *Loop {
	return &Loop{git: gitMgr}
}

func (l *Loop) sweepOnce(ctx context.Context) {
	repos, err := l.git.ListClonedRepos()
	if err != nil {
		slog.Error("sweep: list cloned repos failed", "error", err)
		return
	}
	for _, repo := range repos {
		if err := l.git.SweepGoneBranches(ctx, repo); err != nil {
			slog.Error("sweep: repo failed", "repo", repo, "error", err)
		}
	}
}

func (l *Loop) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		l.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
