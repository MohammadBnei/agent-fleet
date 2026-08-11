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
		mcp.WithDescription("Run a shell command in the e2e pod's worktree (bash -lc), for anything the app needs beyond its initial start: installing a missed dependency, restarting the dev server, running the app's own tests, inspecting a failure. Returns stdout, stderr, and exit code — a nonzero exit is not an error, it's information. A long-running process (e.g. a dev server) should background itself (trailing &) rather than block this call; timeout is 5 minutes."),
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
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		exitCode := 0
		if runErr := cmd.Run(); runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				// Didn't even start/finish (bad binary, timeout kill) — a
				// genuine tool-call error, unlike a nonzero exit from a
				// command that ran to completion.
				return mcp.NewToolResultError(fmt.Sprintf("run_command: %v", runErr)), nil
			}
		}

		result, err := json.Marshal(map[string]any{
			"stdout":   stdout.String(),
			"stderr":   stderr.String(),
			"exitCode": exitCode,
		})
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(result)), nil
	}
}
