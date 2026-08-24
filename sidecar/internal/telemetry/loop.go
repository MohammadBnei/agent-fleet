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
	"path/filepath"
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
// stat, not a substitute for reviewing the actual diff. The response carries
// back the paths a human has opened in the console and wants a real diff for;
// those are computed and attached to the NEXT push.
func Run(ctx context.Context, core *coreclient.Client, worktreePath string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastBody string
	var wanted []string
	for {
		lastBody, wanted = push(ctx, core, worktreePath, lastBody, wanted)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// push sends a snapshot identical to lastBody as an EMPTY summary rather than
// re-sending it — the diff sits unchanged for many ticks while the agent is
// mid-thought, and core appends a transcript row per non-empty summary
// (ToolCallLine has no render-side dedup, and Append is a pure insert). The
// call itself still happens on every tick, which is the change: it is the only
// channel core has for asking this pod anything, so skipping it entirely — as
// this did — meant a diff request could sit unanswered for as long as the agent
// was quiet, which is exactly when a human is most likely to be reading.
//
// Returns the body actually represented (or lastBody unchanged) and the paths
// core wants diffed next, both carried into the following tick.
func push(ctx context.Context, core *coreclient.Client, worktreePath, lastBody string, wanted []string) (string, []string) {
	s, err := computeSummary(worktreePath)
	if err != nil {
		slog.Warn("telemetry: compute diff summary failed", "error", err)
		return lastBody, nil
	}

	body := ""
	if len(s.Files) > 0 {
		marshalled, err := json.Marshal(s)
		if err != nil {
			slog.Warn("telemetry: marshal summary failed", "error", err)
			return lastBody, nil
		}
		body = string(marshalled)
	}

	// What core actually gets: the summary only when it is new. Everything
	// else on this call is the diff exchange.
	summary := body
	if summary == lastBody {
		summary = ""
	}

	nextWanted, err := core.PushToolTelemetry(ctx, summary, diffsJSON(worktreePath, wanted))
	if err != nil {
		slog.Warn("telemetry: push failed", "error", err)
		return lastBody, nil
	}
	if summary != "" {
		lastBody = body
	}
	return lastBody, nextWanted
}

// maxDiffBytes caps one file's diff. Mirrors core/internal/filediff's own cap:
// a generated file or a lockfile rewrite is precisely what gets clicked here,
// and neither end should be the one that discovers a megabyte is on the wire.
const maxDiffBytes = 256 * 1024

// diffsJSON runs `git diff -- <path>` for each wanted path. An empty result is
// kept and sent: it is a real answer (the file was committed, reverted, or was
// never tracked) and it is what lets the console stop polling.
func diffsJSON(worktreePath string, wanted []string) string {
	if len(wanted) == 0 {
		return ""
	}
	out := make(map[string]string, len(wanted))
	for _, path := range wanted {
		if !safeRelPath(path) {
			// Not an error worth failing the tick over, but not a path this
			// hands to git either.
			slog.Warn("telemetry: refusing unsafe diff path", "path", path)
			continue
		}
		// `--` is what stops a path that happens to match a ref being read as
		// one. It is already positional here, but the separator is the part
		// that makes that true rather than incidental.
		diff, err := gitOutput(worktreePath, "diff", "--", path)
		if err != nil {
			// git exits non-zero for a path it does not know. Empty is the
			// honest answer and ends the poll; an omission leaves the console
			// waiting forever.
			slog.Warn("telemetry: diff failed", "path", path, "error", err)
			diff = ""
		}
		if len(diff) > maxDiffBytes {
			diff = diff[:maxDiffBytes] + "\n… diff truncated\n"
		}
		out[path] = diff
	}
	if len(out) == 0 {
		return ""
	}
	body, err := json.Marshal(out)
	if err != nil {
		slog.Warn("telemetry: marshal diffs failed", "error", err)
		return ""
	}
	return string(body)
}

// safeRelPath keeps this loop from running git against anything but a file
// inside the worktree. The paths come from core, which got them from a browser,
// so they are attacker-shaped by default even though the only caller today is
// the console's own CHANGES panel echoing back a path numstat produced.
//
// `git diff -- ../../etc/passwd` is refused by git itself ("outside repository"),
// so this is defence in depth rather than the only guard — but the day someone
// swaps this for `git show` or adds a --no-index flag, the guard is what is
// left.
func safeRelPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "-") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return false
		}
	}
	return true
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
