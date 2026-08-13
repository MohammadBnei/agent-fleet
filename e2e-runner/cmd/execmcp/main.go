// Command execmcp is the e2e pod's first-party MCP listener: a single
// run_command tool that lets the agent run ad-hoc shell commands against
// the live preview pod (install a missed dependency, restart the dev
// server, run the app's own tests) instead of the provisioner guessing a
// single start command once at pod-creation time. Reuses the same
// mark3labs/mcp-go server library sidecar's own MCP server is built with.
// Proxied to by provisioner/internal/mcpproxy — never reached directly by
// the agent (docs/adr/0020 hub-and-spoke: MCP is local-only to the
// sidecar, this is a provisioner-mediated hop, same as Playwright calls).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// commandTimeout bounds a single run_command call — a caller wanting a
// long-running process (a dev server) backgrounds it itself inside the
// command string, same convention entrypoint.sh already uses for the app.
// 15 minutes, not 5: this pod is the worker's build/test sandbox
// (docs/adr/0039), and a cold `bun install` on the shared cache measured
// 782s live (docs/adr/0036) — the one command the sandbox most exists to
// run was longer than its own timeout. Nothing upstream bounds this;
// core's sessionCallTimeout covers CreateWorkerPod/TearDownSession only.
const commandTimeout = 15 * time.Minute

// maxStreamBytes caps how much of each stream reaches the agent's context.
// 15000 per stream ≈ Claude Code's own 30000-char Bash ceiling in
// aggregate, and well inside the 10000-token MAX_MCP_OUTPUT_TOKENS the
// provisioner sets — that CLI-side cap is a blunt tail-chop with no way
// back to the lost bytes, so cutting here first means the agent gets a
// pointer instead of a cliff (ADR-0046).
//
// This is a build/test sandbox: `bun install` and a verbose test suite both
// produce far more than this, and under ADR-0039 all of it lands in a
// session that never sheds it (MCP results are not microcompactable).
const maxStreamBytes = 15000

// headBuffer keeps the first max bytes written to it and counts the rest.
// Head rather than tail: a build failure's first error is the real one, and
// the full stream is preserved on disk anyway.
type headBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	room := h.max - h.buf.Len()
	switch {
	case room <= 0:
		h.dropped += len(p)
	case len(p) <= room:
		h.buf.Write(p)
	default:
		h.buf.Write(p[:room])
		h.dropped += len(p) - room
	}
	// Always claim the full write. A short count makes io.MultiWriter treat
	// this as ErrShortWrite and abort the whole fan-out, which would take
	// the on-disk log down with it — the exact thing the truncation notice
	// promises is still there.
	return len(p), nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	port := flag.String("port", "", "port to listen on")
	flag.Parse()
	if *port == "" {
		slog.Error("--port is required")
		os.Exit(1)
	}

	worktreePath := os.Getenv("E2E_WORKTREE_PATH")
	if worktreePath == "" {
		slog.Error("E2E_WORKTREE_PATH is required")
		os.Exit(1)
	}

	s := server.NewMCPServer("agent-fleet-e2e-exec", "0.1.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("run_command",
		mcp.WithDescription("Run a shell command in the e2e pod's worktree (bash -lc): installing a dependency, restarting the dev server, running tests, inspecting a failure. Returns stdout, stderr, exitCode — a nonzero exit is information, not an error. Background long-running processes with a trailing &; timeout 15 minutes. Large output is truncated and the full log's path returned in fullOutputPath — tail/grep it with this same tool."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command, run via bash -lc from the worktree root")),
	), runCommandHandler(worktreePath))

	httpServer := &http.Server{Addr: ":" + *port, Handler: server.NewStreamableHTTPServer(s)}
	slog.Info("execmcp listening", "port", *port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("execmcp server exited", "error", err)
		os.Exit(1)
	}
}

func runCommandHandler(worktreePath string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command := req.GetString("command", "")
		if command == "" {
			return mcp.NewToolResultError("command is required"), nil
		}

		runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()

		cmd := exec.CommandContext(runCtx, "bash", "-lc", command)
		cmd.Dir = worktreePath
		stdout := &headBuffer{max: maxStreamBytes}
		stderr := &headBuffer{max: maxStreamBytes}
		// The full, interleaved stream also goes to a file in the sandbox,
		// which is what makes truncation safe: /tmp persists across
		// run_command calls in this container (docs/adr/0044 already relies
		// on that for /tmp/e2e-app.log), so the agent recovers the dropped
		// bytes with an ordinary `tail`/`grep` through this same tool. No new
		// MCP tool, no new RPC, no pagination cursor to maintain.
		//
		// Interleaving is real here in a way the two separate buffers can't
		// be: both streams share one file handle, so the log preserves the
		// order the command actually produced.
		var logPath string
		if logFile, err := os.CreateTemp("/tmp", "run-*.log"); err != nil {
			// Non-fatal: the command still runs and still returns. The agent
			// is told the rest is unrecoverable rather than being handed a
			// path that isn't there.
			slog.Warn("run_command: could not open full-output log", "error", err)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
		} else {
			// Not `_ = Close()`: this file is the recovery path the
			// truncation notice promises, so a failed close is the one case
			// where the agent may have been handed a path to incomplete
			// content. Worth a line in the sandbox's own logs.
			defer func() {
				if cerr := logFile.Close(); cerr != nil {
					slog.Warn("run_command: closing full-output log failed", "path", logFile.Name(), "error", cerr)
				}
			}()
			logPath = logFile.Name()
			cmd.Stdout = io.MultiWriter(stdout, logFile)
			cmd.Stderr = io.MultiWriter(stderr, logFile)
		}
		// Without this, `run_command 'bun run dev &'` blocks for the full
		// commandTimeout and then still doesn't return: Stdout/Stderr are
		// buffers rather than *os.File, so os/exec wires real OS pipes and
		// Wait blocks until every writer closes them — and a backgrounded
		// grandchild inherits those pipes and holds them open for its whole
		// life. CommandContext's kill on timeout only reaps the already-dead
		// bash, not the grandchild, so the copy goroutine keeps waiting.
		//
		// That made the trailing-& convention this tool's own description
		// tells the agent to use (and that docs/adr/0044 relies on for
		// restarting a dead app) silently unusable.
		cmd.WaitDelay = time.Second

		exitCode := 0
		if runErr := cmd.Run(); runErr != nil {
			var exitErr *exec.ExitError
			switch {
			case errors.As(runErr, &exitErr):
				exitCode = exitErr.ExitCode()
			case errors.Is(runErr, exec.ErrWaitDelay):
				// The command itself succeeded; only the inherited pipes were
				// still open (the backgrounded-child case above). Report the
				// output captured so far as an ordinary success.
			default:
				// Didn't even start/finish (bad binary, timeout kill) — a
				// genuine tool-call error, unlike a nonzero exit from a
				// command that ran to completion.
				return mcp.NewToolResultError(fmt.Sprintf("run_command: %v", runErr)), nil
			}
		}

		payload := map[string]any{
			"stdout":   stdout.buf.String(),
			"stderr":   stderr.buf.String(),
			"exitCode": exitCode,
		}
		// A truncation the agent isn't told about is worse than no
		// truncation: it works from partial build output believing it saw
		// everything. Wording deliberately mirrors Claude Code's own MCP
		// truncation notice, which tells the model to reach for the server's
		// pagination/filtering tools — here that's this same tool plus a path.
		if dropped := stdout.dropped + stderr.dropped; dropped > 0 {
			payload["truncated"] = true
			payload["droppedBytes"] = dropped
			if logPath == "" {
				payload["note"] = fmt.Sprintf(
					"[OUTPUT TRUNCATED - %d bytes dropped] The full output could not be saved, so the rest is unrecoverable. Treat these results as incomplete and say so.", dropped)
			} else {
				payload["fullOutputPath"] = logPath
				payload["note"] = fmt.Sprintf(
					"[OUTPUT TRUNCATED - first %d bytes per stream kept, %d bytes dropped] The complete interleaved stdout+stderr is at %s in this sandbox. Retrieve the rest with run_command, e.g. `tail -n 200 %s` or `grep -n error %s`. Do not conclude the command succeeded or failed on the truncated text alone if the answer isn't in it.",
					maxStreamBytes, dropped, logPath, logPath, logPath)
			}
		}
		result, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(result)), nil
	}
}
