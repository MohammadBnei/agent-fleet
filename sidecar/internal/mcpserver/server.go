// Package mcpserver hosts the sidecar's local (localhost-only) MCP server —
// the Agent SDK session's mcpServers config points here, never at core
// directly (docs/adr/0020/0021: MCP is purely local, gRPC is the only
// inter-pod protocol). Every tool here proxies onward to core over the
// sidecar's one outbound gRPC connection. Tool contracts are preserved
// verbatim from core/internal/coreserver's old fleet-core mcpserver and
// provisioner's old per-task mcpserver — just without a taskId parameter,
// since one sidecar only ever serves the one task its pod was created for.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type AskUserQuestionOption struct {
	Label       string `json:"label" jsonschema_description:"Short label for this option (1-5 words)"`
	Description string `json:"description" jsonschema_description:"What this option means or its tradeoffs"`
}

type AskUserQuestionQuestion struct {
	Question    string                  `json:"question" jsonschema_description:"The complete question to ask, ending with a question mark"`
	Header      string                  `json:"header" jsonschema_description:"Very short label shown as a chip/tag, max 12 chars"`
	Options     []AskUserQuestionOption `json:"options" jsonschema_description:"2-4 mutually exclusive choices"`
	MultiSelect bool                    `json:"multiSelect,omitempty" jsonschema_description:"Set true to allow selecting more than one option"`
}

type AskUserQuestionArgs struct {
	Questions []AskUserQuestionQuestion `json:"questions" jsonschema_description:"1-4 questions to ask the human"`
	TimeoutMs int                       `json:"timeoutMs,omitempty" jsonschema_description:"How long to block waiting for an answer before returning {\"status\":\"pending\"} (default 60000). Call the tool again with the same questions to keep waiting."`
}

// New builds the sidecar's local MCP HTTP handler. Playwright's tool set
// (discovered at runtime once an e2e session is live) is registered lazily
// by request_e2e_env's handler, mirroring the exact dynamic-registration +
// notifications/tools/list_changed pattern provisioner's old per-task
// mcpserver already used.
func New(core *coreclient.Client) http.Handler {
	s := server.NewMCPServer("agent-fleet-sidecar", "0.1.0", server.WithToolCapabilities(true))

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Append a message to this task's shared transcript (visible to the human via the dashboard/Discord relay). The response's nextIndex is the sinceIndex to use if you ever call wait_for_messages afterward — see that tool's own description for why."),
		mcp.WithString("from", mcp.Required(), mcp.Description("'agent' | 'human'")),
		mcp.WithString("text", mcp.Required()),
		mcp.WithString("type", mcp.Description("'discussion' | 'approve' | 'abort'")),
		mcp.WithString("idempotencyKey"),
	), sendMessageHandler(core))

	s.AddTool(mcp.NewTool("wait_for_messages",
		mcp.WithDescription("Block (up to timeoutMs) until new transcript messages appear at or after sinceIndex, then return them. You almost never need this: new human messages already arrive live as your next input while a session is running, with no polling required. sinceIndex is INCLUSIVE and results are NOT filtered by from — if you pass the raw index a prior send_message call returned, you will see that same message come back to you. Always pass the nextIndex field from your last send_message or wait_for_messages response instead."),
		mcp.WithNumber("sinceIndex"),
		mcp.WithNumber("timeoutMs"),
	), waitForMessagesHandler(core))

	s.AddTool(mcp.NewTool("AskUserQuestion",
		mcp.WithDescription("Ask the human one or more structured multiple-choice questions. Answered via the web dashboard. Blocks (up to timeoutMs) until answered, or returns {\"status\":\"pending\"} if it times out first (call again with the same questions to keep waiting). See docs/adr/0018."),
		mcp.WithInputSchema[AskUserQuestionArgs](),
	), askUserQuestionHandler(core))

	s.AddTool(mcp.NewTool("request_e2e_env",
		mcp.WithDescription("Request an on-demand e2e test environment for this task: a live pod running the app on this branch, code-server for human review, and a Playwright MCP server. Safe to call again if one is already running — returns the existing session's URL. The response echoes back the recipe that was actually used (resolvedStartCmd, profileName, tools, services), which comes from the repo's dashboard-editable e2e profile. If the preview does not serve, read resolvedStartCmd and report what is wrong with it — do not silently substitute your own command. IMPORTANT: Cold dependency install may take 10+ minutes. The preview URL won't serve until the app starts. You can check progress with `run_command 'ps aux'` or similar commands."),
		mcp.WithString("startCmd", mcp.Description("Optional override for the profile's start command, applied to THIS TASK ONLY and never saved back to the profile. Requires a human to approve it before anything is created — if they decline or don't answer, the profile's own command is used instead. Omit it unless you have concrete evidence the profile is wrong for this worktree; the profile is the default for good reason. Whatever runs must bind 0.0.0.0 on $PORT — a dev server left on its own default port, or bound to localhost, is unreachable from outside the pod and the preview never serves.")),
	), requestE2eEnvHandler(core, s))

	s.AddTool(mcp.NewTool("kill_env",
		mcp.WithDescription("Permanently destroy this task's e2e environment. WARNING: Do NOT use to 'refresh' the app after editing files - the shared worktree hot-reloads automatically, just reload the preview URL in your browser. Only use kill_env when completely done with testing. Recreating the environment wastes 10+ minutes on cold dependency install."),
	), killEnvHandler(core))

	// Registered statically, unlike the Playwright tools below it: this is
	// the worker's build/test sandbox (docs/adr/0039), so it has to exist
	// from the session's first turn rather than appear only after a
	// request_e2e_env call — which also meant it never came back at all on a
	// resumed session, and depended on notifications/tools/list_changed
	// being honored (a risk docs/adr/0012 flagged and never verified).
	s.AddTool(mcp.NewTool("run_command",
		mcp.WithDescription("Run a shell command in this task's sandbox — a separate pod that already has the repo's toolchain, its services (Postgres/Redis), a warm dependency cache, and the app's dev server, sharing this task's worktree. Prefer this over Bash for anything that builds, tests, lints, installs dependencies, or runs the app. Starts the sandbox on first use if it isn't running yet, so no separate setup call is needed. Returns stdout, stderr and exit code — a nonzero exit is not an error, it's information. The sandbox has NO git and no GitHub credentials on purpose: run git, gh and anything that opens the PR through Bash instead. A long-running process (e.g. a dev server) should background itself (trailing &) rather than block this call; timeout is 15 minutes."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command, run via bash -lc from the worktree root")),
	), runCommandHandler(core, s))

	s.AddTool(mcp.NewTool("list_shared_files",
		mcp.WithDescription("List files in the fleet-wide shared file space (a single flat Garage S3 bucket, shared by every task and by the human via the dashboard). Returns each file's key, size, last-modified time, and content type."),
	), listSharedFilesHandler(core))

	s.AddTool(mcp.NewTool("get_shared_file_upload_url",
		mcp.WithDescription("Get a short-lived presigned URL to upload a file into the fleet-wide shared file space. The filename becomes the object's key verbatim (flat namespace, last write wins). This tool does not move any bytes — after calling it, upload the actual file from Bash: curl -T <local-path> \"<uploadUrl>\"."),
		mcp.WithString("filename", mcp.Required()),
		mcp.WithString("contentType"),
	), getSharedFileUploadURLHandler(core))

	s.AddTool(mcp.NewTool("get_shared_file_download_url",
		mcp.WithDescription("Get a short-lived presigned URL to download a file from the fleet-wide shared file space by its key (see list_shared_files). This tool does not move any bytes — after calling it, download from Bash: curl -o <local-path> \"<downloadUrl>\"."),
		mcp.WithString("key", mcp.Required()),
	), getSharedFileDownloadURLHandler(core))

	s.AddTool(mcp.NewTool("delete_shared_file",
		mcp.WithDescription("Delete a file from the fleet-wide shared file space by its key."),
		mcp.WithString("key", mcp.Required()),
	), deleteSharedFileHandler(core))

	s.AddTool(mcp.NewTool("journal_write",
		mcp.WithDescription("Write a note to this repo's knowledge journal — something worth a future session on the same repo knowing (a gotcha, a decision, a dead end). Not for routine progress updates; use send_message for those. Searchable later via journal_search."),
		mcp.WithString("repo", mcp.Required()),
		mcp.WithString("note", mcp.Required()),
	), journalWriteHandler(core))

	s.AddTool(mcp.NewTool("journal_search",
		mcp.WithDescription("Search this repo's knowledge journal for past entries relevant to a query (Postgres full-text search, ranked by relevance). Use before starting non-trivial work on a repo to check for prior learnings."),
		mcp.WithString("repo", mcp.Required()),
		mcp.WithString("query", mcp.Required()),
		mcp.WithNumber("limit"),
	), journalSearchHandler(core))

	// Inter-agent coordination (docs/adr/0041). Registered statically for
	// the same reason run_command is: a resumed session must have them from
	// turn one, not only after some other call happens to register them.
	s.AddTool(mcp.NewTool("list_sessions",
		mcp.WithDescription("List the fleet's other sessions — task id, repo, description, status, and liveState. liveState is what tells you whether a session can be talked to: 'working' (busy), 'blocked' (waiting on a HUMAN decision — you cannot prompt it and must not try to answer for it), 'idle'/'done' (finished), 'stalled' (owes a reply and hasn't produced one), 'unknown' (pod starting), or '' (no live pod — prompting one warms it). Your own session is never listed."),
	), listSessionsHandler(core))

	s.AddTool(mcp.NewTool("prompt_agent",
		mcp.WithDescription("Send a message to another session, as yourself. It arrives in that session's transcript attributed to you, and warms its pod if it was idle so it is actually read. Use this to hand off work or ask a question of a session that owns a different repo — not to chat. The target sees your message the same way it sees a human's, so say what you need concretely. REFUSED if the target is 'blocked': it is waiting on a human decision, and that decision is not yours to resolve. Also refused for your own session, and beyond a small relay depth so chains cannot loop. To wait for a reply, follow with wait_for_agent."),
		mcp.WithString("taskId", mcp.Required(), mcp.Description("Target session's task id, from list_sessions")),
		mcp.WithString("text", mcp.Required()),
	), promptSessionHandler(core))

	s.AddTool(mcp.NewTool("wait_for_agent",
		mcp.WithDescription("Block until another session reaches a given liveness state, or the timeout expires. With no `until`, waits for it to settle (idle, done, blocked or stalled). `until: blocked` is the useful one after prompting: it returns when that session genuinely needs a human, which is the moment worth surfacing. A timeout is not an error — the response says timedOut with the state it actually reached, and 'still working' is a legitimate answer to act on."),
		mcp.WithString("taskId", mcp.Required()),
		mcp.WithString("until", mcp.Description("working | blocked | idle | done | stalled | unknown. Omit to wait for any settled state.")),
		mcp.WithNumber("timeoutMs", mcp.Description("Default 120000.")),
	), waitForSessionHandler(core))

	s.AddTool(mcp.NewTool("view_logs",
		mcp.WithDescription("View recent logs from fleet components or deployed apps. Returns formatted log entries. Use this to debug issues with worker, sidecar, or the deployed application during e2e tests. Supports both duration-based queries (duration) and explicit time ranges (start_time/end_time in RFC3339 format)."),
		mcp.WithString("component", mcp.Required(), mcp.Description("Which component: worker|sidecar|core|provisioner|e2e|app")),
		mcp.WithString("app_name", mcp.Description("For component=app, specify app name: dream-analyst|vos-monolith")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace (default: agent-fleet, use 'default' for deployed apps)")),
		mcp.WithString("level", mcp.Description("Filter by level: debug|info|warn|error (default: all)")),
		mcp.WithString("duration", mcp.Description("How far back: 1h|30m|6h|24h (default: 1h) - ignored if start_time is set")),
		mcp.WithNumber("limit", mcp.Description("Max entries to return (default 50, max 1000)")),
		mcp.WithString("start_time", mcp.Description("Optional: RFC3339 timestamp (e.g., '2024-01-15T10:30:00Z') - overrides duration")),
		mcp.WithString("end_time", mcp.Description("Optional: RFC3339 timestamp (default: now)")),
	), viewLogsHandler(core))

	return server.NewStreamableHTTPServer(s)
}

// sendMessageHandler writes one transcript entry through core and returns
// the cursor a caller should resume from.
func sendMessageHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		from := req.GetString("from", "")
		text := req.GetString("text", "")
		msgType := req.GetString("type", "")
		idempotencyKey := req.GetString("idempotencyKey", "")
		if from == "" || text == "" {
			return mcp.NewToolResultError("from and text are required"), nil
		}
		slog.Info("mcp send_message", "from", from, "type", msgType)
		seq, err := core.SendMessage(ctx, from, text, stringToProtoType(msgType), idempotencyKey)
		if err != nil {
			slog.Error("mcp send_message", "error", err)
			return nil, fmt.Errorf("send_message: %w", err)
		}
		// nextIndex (seq+1) is the exclusive cursor a caller should use for
		// wait_for_messages' sinceIndex — that tool's own sinceIndex is
		// inclusive, so passing this message's raw seq back would echo this
		// same message to its own sender.
		body, _ := json.Marshal(map[string]int64{"index": seq, "nextIndex": seq + 1})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func waitForMessagesHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sinceIndex := int64(req.GetInt("sinceIndex", 0))
		timeoutMs := int32(req.GetInt("timeoutMs", 30000))
		slog.Debug("mcp wait_for_messages", "sinceIndex", sinceIndex, "timeoutMs", timeoutMs)
		entries, nextSeq, err := core.WaitForMessages(ctx, sinceIndex, timeoutMs)
		if err != nil {
			slog.Error("mcp wait_for_messages", "error", err)
			return nil, fmt.Errorf("wait_for_messages: %w", err)
		}
		messages := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			messages = append(messages, map[string]any{"from": e.GetFrom(), "text": e.GetText(), "type": protoTypeToString(e.GetType())})
		}
		body, _ := json.Marshal(map[string]any{"messages": messages, "nextIndex": nextSeq})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func journalWriteHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo := req.GetString("repo", "")
		note := req.GetString("note", "")
		if repo == "" || note == "" {
			return mcp.NewToolResultError("repo and note are required"), nil
		}
		payload, err := json.Marshal(map[string]string{"note": note})
		if err != nil {
			return nil, fmt.Errorf("journal_write: marshal note: %w", err)
		}
		slog.Info("mcp journal_write", "repo", repo)
		if err := core.AppendJournal(ctx, repo, "worker", "agent_note", string(payload)); err != nil {
			slog.Error("mcp journal_write", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(`{"status":"written"}`), nil
	}
}

func journalSearchHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo := req.GetString("repo", "")
		query := req.GetString("query", "")
		if repo == "" || query == "" {
			return mcp.NewToolResultError("repo and query are required"), nil
		}
		limit := int32(req.GetInt("limit", 20))
		slog.Info("mcp journal_search", "repo", repo, "query", query)
		entries, err := core.SearchJournal(ctx, repo, query, limit)
		if err != nil {
			slog.Error("mcp journal_search", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"actor": e.GetActor(), "eventType": e.GetEventType(),
				"payloadJson": e.GetPayloadJson(), "createdAt": e.GetCreatedAt(),
			})
		}
		body, _ := json.Marshal(map[string]any{"entries": out})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func askUserQuestionHandler(core *coreclient.Client) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewTypedToolHandler(func(ctx context.Context, _ mcp.CallToolRequest, args AskUserQuestionArgs) (*mcp.CallToolResult, error) {
		if len(args.Questions) == 0 {
			return mcp.NewToolResultError("at least one question is required"), nil
		}
		payload, err := json.Marshal(map[string]any{"questions": args.Questions})
		if err != nil {
			return nil, fmt.Errorf("AskUserQuestion: marshal questions: %w", err)
		}
		timeoutMs := int32(args.TimeoutMs)
		if timeoutMs <= 0 {
			timeoutMs = 60000
		}
		slog.Info("mcp AskUserQuestion", "questions", len(args.Questions))
		status, answersJSON, _, err := core.AskUserQuestion(ctx, string(payload), timeoutMs)
		if err != nil {
			slog.Error("mcp AskUserQuestion", "error", err)
			return nil, fmt.Errorf("AskUserQuestion: %w", err)
		}
		if status == "answered" {
			return mcp.NewToolResultText(answersJSON), nil
		}
		body, _ := json.Marshal(map[string]string{"status": "pending"})
		return mcp.NewToolResultText(string(body)), nil
	})
}

// startCmdOverrideQuestion and its two option labels are the wire contract
// between this gate and the dashboard's answer payload
// ({"answers": {"<question>": "<label>"}}), so they're constants rather than
// inline strings — a typo here reads as "declined", which is the safe way to
// fail but a confusing one to debug.
const (
	startCmdOverrideQuestion = "The agent wants to start this task's e2e app with a command that differs from the repo's configured e2e profile. Which one should run?"
	startCmdKeepProfile      = "Use the profile"
	startCmdUseOverride      = "Use the agent's"
)

// questionAsker is the slice of coreclient.Client confirmStartCmdOverride
// needs — same test-seam pattern viewLogsHandler already uses in this
// package, so the gate's decision logic is unit-testable without bufconn.
type questionAsker interface {
	AskUserQuestion(ctx context.Context, questionsJSON string, timeoutMs int32) (status, answersJSON string, questionSeq int64, err error)
}

// confirmStartCmdOverride blocks on a real human answer before an agent's
// start_cmd is allowed to beat the repo's profile. Anything other than an
// explicit pick of the override — decline, timeout, malformed answer —
// means the profile wins, matching docs/adr/0029's rule that a decision is
// never inferred from silence. Enforced here rather than left to the agent
// to ask: an agent choosing whether to check is exactly how a guessed
// command silently replaced a correct profile and 502'd the preview.
func confirmStartCmdOverride(ctx context.Context, core questionAsker, startCmd string) bool {
	payload, err := json.Marshal(map[string]any{"questions": []AskUserQuestionQuestion{{
		Question: startCmdOverrideQuestion,
		Header:   "e2e cmd",
		Options: []AskUserQuestionOption{
			{Label: startCmdKeepProfile, Description: "Run the repo's configured e2e profile command, as edited in the dashboard. Ignores the agent's proposal."},
			{Label: startCmdUseOverride, Description: "Run this instead, for this task only (the profile is not modified): " + startCmd},
		},
	}}})
	if err != nil {
		slog.Error("mcp request_e2e_env: marshal override question", "error", err)
		return false
	}
	status, answersJSON, _, err := core.AskUserQuestion(ctx, string(payload), 300000)
	if err != nil {
		slog.Error("mcp request_e2e_env: override question", "error", err)
		return false
	}
	if status != "answered" {
		slog.Info("mcp request_e2e_env: override not approved", "status", status)
		return false
	}
	var parsed struct {
		Answers map[string]string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(answersJSON), &parsed); err != nil {
		slog.Error("mcp request_e2e_env: parse override answer", "error", err)
		return false
	}
	return parsed.Answers[startCmdOverrideQuestion] == startCmdUseOverride
}

func requestE2eEnvHandler(core *coreclient.Client, s *server.MCPServer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		startCmd := req.GetString("startCmd", "")
		slog.Info("mcp request_e2e_env", "startCmdOverrideProposed", startCmd != "")
		if startCmd != "" && !confirmStartCmdOverride(ctx, core, startCmd) {
			startCmd = ""
		}
		resp, err := core.RequestE2eEnv(ctx, startCmd)
		if err != nil {
			slog.Error("mcp request_e2e_env", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		// New Playwright tools just became reachable — register them and
		// tell the client the tool list changed (see docs/adr/0012's
		// flagged risk: verify this is actually honored by the Agent SDK).
		tools, err := core.ListE2eTools(ctx)
		if err == nil {
			for _, t := range tools {
				// A provisioner still on the old image lists run_command
				// here; registering it would overwrite New()'s static,
				// pod-provisioning handler with a plain passthrough. The
				// two run as separate images, so this window is real on
				// every rolling deploy, not just theoretical.
				if t.GetName() == runCommandToolName {
					continue
				}
				addProxiedTool(s, core, t)
			}
			s.SendNotificationToAllClients("notifications/tools/list_changed", nil)
		}

		body, _ := json.Marshal(map[string]any{
			"url":              resp.GetPreviewUrl(),
			"resolvedStartCmd": resp.GetResolvedStartCmd(),
			"profileName":      resp.GetProfileName(),
			"tools":            resp.GetTools(),
			"services":         resp.GetServices(),
		})
		return mcp.NewToolResultText(string(body)), nil
	}
}

// runCommandToolName is the tool served by the e2e pod's own execmcp
// listener, proxied through core (provisioner/internal/mcpproxy routes it).
const runCommandToolName = "run_command"

// e2eRunner is the slice of coreclient.Client runCommandHandler needs —
// same test-seam pattern viewLogsHandler and confirmStartCmdOverride
// already use in this package, so the retry logic is unit-testable without
// bufconn.
type e2eRunner interface {
	CallE2eTool(ctx context.Context, toolName, argumentsJSON string) (resultJSON string, isError bool, err error)
	RequestE2eEnv(ctx context.Context, startCmd string) (*agentfleetv1.RequestE2EEnvResponse, error)
}

// runCommandHandler runs the command in this task's e2e pod, provisioning
// one first if there isn't a live one yet.
//
// Ordering is deliberate: call first, provision only if the call fails.
// RequestE2eEnv is idempotent (CreateE2eSession short-circuits on an
// existing pod), so provisioning up-front would have worked too — but it
// would put two extra gRPC hops and a Postgres profile lookup in front of
// every single command, to fix a state that only exists once per session.
// Doing it on failure also self-heals after kill_env, which a
// provisioned-once flag would not.
func runCommandHandler(core e2eRunner, s *server.MCPServer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command := req.GetString("command", "")
		if command == "" {
			return mcp.NewToolResultError("command is required"), nil
		}
		argsJSON, err := json.Marshal(map[string]any{"command": command})
		if err != nil {
			return nil, fmt.Errorf("marshal run_command arguments: %w", err)
		}

		resultJSON, isError, err := core.CallE2eTool(ctx, runCommandToolName, string(argsJSON))
		if err == nil {
			return e2eToolResult(resultJSON, isError, "")
		}

		// execmcp reports a nonzero exit as an ordinary result, never as an
		// error, so reaching here means the call didn't land at all — no
		// live sandbox pod. Start one and try once more.
		slog.Info("mcp run_command: no live sandbox, provisioning one", "error", err)
		env, provErr := core.RequestE2eEnv(ctx, "")
		if provErr != nil {
			slog.Error("mcp run_command: provisioning failed", "error", provErr)
			return mcp.NewToolResultError(fmt.Sprintf("run_command: no sandbox running and one could not be started: %v", provErr)), nil
		}

		// Ensure Playwright tools registered for resumed sessions where
		// notifications/tools/list_changed doesn't re-fire (ADR-0039).
		if coreClient, ok := core.(*coreclient.Client); ok {
			ensureE2eToolsRegistered(ctx, s, coreClient)
		}

		resultJSON, isError, err = core.CallE2eTool(ctx, runCommandToolName, string(argsJSON))
		if err != nil {
			slog.Error("mcp run_command: still unreachable after provisioning", "error", err)
			return mcp.NewToolResultError(fmt.Sprintf("run_command: sandbox started but is not reachable: %v", err)), nil
		}
		// Tell the agent what it just landed in. Without this the sandbox
		// appears out of nowhere with an unknown toolchain and an unknown
		// start command — the same blindness docs/adr/0036 added
		// resolvedStartCmd to request_e2e_env's response to fix.
		recipe, _ := json.Marshal(map[string]any{
			"startedSandbox":   true,
			"previewUrl":       env.GetPreviewUrl(),
			"profileName":      env.GetProfileName(),
			"resolvedStartCmd": env.GetResolvedStartCmd(),
			"tools":            env.GetTools(),
			"services":         env.GetServices(),
		})
		return e2eToolResult(resultJSON, isError, string(recipe))
	}
}

// e2eToolResult rebuilds the proxied CallToolResult, optionally prefixing a
// note of its own as an extra leading text block.
func e2eToolResult(resultJSON string, isError bool, note string) (*mcp.CallToolResult, error) {
	var result mcp.CallToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return nil, fmt.Errorf("unmarshal tool result: %w", err)
	}
	result.IsError = isError
	if note != "" {
		result.Content = append([]mcp.Content{mcp.NewTextContent(note)}, result.Content...)
	}
	return &result, nil
}

// addProxiedTool registers one runtime-discovered e2e tool (Playwright's
// set) as a straight passthrough. run_command is deliberately absent from
// what ProxiedTools returns — it's registered statically in New() with a
// handler that can provision a pod, and re-registering it here would
// replace that with a passthrough that can't (docs/adr/0039).
func addProxiedTool(s *server.MCPServer, core *coreclient.Client, desc *agentfleetv1.E2EToolDescriptor) {
	var schema mcp.ToolInputSchema
	_ = json.Unmarshal([]byte(desc.GetInputSchemaJson()), &schema)
	tool := mcp.Tool{Name: desc.GetName(), Description: desc.GetDescription(), InputSchema: schema}
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON, err := json.Marshal(req.GetArguments())
		if err != nil {
			return nil, fmt.Errorf("marshal tool arguments: %w", err)
		}
		resultJSON, isError, err := core.CallE2eTool(ctx, desc.GetName(), string(argsJSON))
		if err != nil {
			return nil, err
		}
		return e2eToolResult(resultJSON, isError, "")
	})
}

// ensureE2eToolsRegistered checks if Playwright tools are registered and
// registers them if missing — fixes resumed sessions where
// notifications/tools/list_changed doesn't re-fire (docs/adr/0039 lines 36-40).
func ensureE2eToolsRegistered(ctx context.Context, s *server.MCPServer, core *coreclient.Client) {
	tools, err := core.ListE2eTools(ctx)
	if err != nil {
		slog.Warn("ensureE2eToolsRegistered failed", "error", err)
		return
	}
	for _, t := range tools {
		if t.GetName() == runCommandToolName {
			continue // run_command already statically registered
		}
		addProxiedTool(s, core, t)
	}
	s.SendNotificationToAllClients("notifications/tools/list_changed", nil)
}

func killEnvHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slog.Info("mcp kill_env")
		if _, err := core.KillE2eEnv(ctx); err != nil {
			slog.Error("mcp kill_env", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("kill requested"), nil
	}
}

func listSharedFilesHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slog.Info("mcp list_shared_files")
		files, err := core.ListFiles(ctx)
		if err != nil {
			slog.Error("mcp list_shared_files", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := make([]map[string]any, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]any{
				"key": f.GetKey(), "sizeBytes": f.GetSizeBytes(),
				"lastModified": f.GetLastModified(), "contentType": f.GetContentType(),
			})
		}
		body, _ := json.Marshal(map[string]any{"files": out})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func getSharedFileUploadURLHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		filename := req.GetString("filename", "")
		contentType := req.GetString("contentType", "")
		if filename == "" {
			return mcp.NewToolResultError("filename is required"), nil
		}
		slog.Info("mcp get_shared_file_upload_url", "filename", filename)
		uploadURL, key, expiresAt, err := core.GetFileUploadURL(ctx, filename, contentType)
		if err != nil {
			slog.Error("mcp get_shared_file_upload_url", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		body, _ := json.Marshal(map[string]string{"uploadUrl": uploadURL, "key": key, "expiresAt": expiresAt})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func getSharedFileDownloadURLHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcp.NewToolResultError("key is required"), nil
		}
		slog.Info("mcp get_shared_file_download_url", "key", key)
		downloadURL, expiresAt, err := core.GetFileDownloadURL(ctx, key)
		if err != nil {
			slog.Error("mcp get_shared_file_download_url", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		body, _ := json.Marshal(map[string]string{"downloadUrl": downloadURL, "expiresAt": expiresAt})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func deleteSharedFileHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		key := req.GetString("key", "")
		if key == "" {
			return mcp.NewToolResultError("key is required"), nil
		}
		slog.Info("mcp delete_shared_file", "key", key)
		if err := core.DeleteFile(ctx, key); err != nil {
			slog.Error("mcp delete_shared_file", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(`{"status":"deleted"}`), nil
	}
}

func stringToProtoType(s string) agentfleetv1.TranscriptEntryType {
	switch s {
	case "discussion":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION
	case "approve":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE
	case "abort":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT
	case "question":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION
	case "answer":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}

func protoTypeToString(t agentfleetv1.TranscriptEntryType) string {
	switch t {
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION:
		return "discussion"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE:
		return "approve"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT:
		return "abort"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION:
		return "question"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER:
		return "answer"
	default:
		return ""
	}
}

// LogViewer is the interface for viewing logs. Implemented by *coreclient.Client and mockable in tests.
type LogViewer interface {
	ViewLogs(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error)
}

func viewLogsHandler(core LogViewer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		component := req.GetString("component", "")
		if component == "" {
			return mcp.NewToolResultError("component is required"), nil
		}

		appName := req.GetString("app_name", "")
		namespace := req.GetString("namespace", "agent-fleet")
		level := req.GetString("level", "")
		duration := req.GetString("duration", "1h")
		limit := int32(req.GetInt("limit", 50))
		startTime := req.GetString("start_time", "")
		endTime := req.GetString("end_time", "")

		slog.Info("mcp view_logs", "component", component, "namespace", namespace, "level", level, "duration", duration, "start_time", startTime, "end_time", endTime)

		logsText, err := core.ViewLogs(ctx, component, appName, namespace, level, duration, limit, startTime, endTime)
		if err != nil {
			slog.Error("mcp view_logs", "error", err)
			return nil, fmt.Errorf("view_logs: %w", err)
		}

		return mcp.NewToolResultText(logsText), nil
	}
}
