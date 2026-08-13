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
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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

// New builds the sidecar's local MCP HTTP handler. Every tool the agent will
// ever see is registered here, at startup — including Playwright's set, from
// an embedded snapshot (docs/adr/0044). Nothing is discovered at runtime and
// nothing depends on notifications/tools/list_changed; a tool the agent
// cannot see is a tool that does not exist, and lazy registration lost that
// race three different ways. See registerPlaywrightTools.
func New(core *coreclient.Client) http.Handler {
	s := server.NewMCPServer("agent-fleet-sidecar", "0.1.0", server.WithToolCapabilities(true))

	s.AddTool(mcp.NewTool("send_message",
		mcp.WithDescription("Append a message to this task's shared transcript (visible to the human via dashboard/Discord). The response's nextIndex is the sinceIndex for wait_for_messages."),
		mcp.WithString("from", mcp.Required(), mcp.Description("'agent' | 'human'")),
		mcp.WithString("text", mcp.Required()),
		mcp.WithString("type", mcp.Description("'discussion' | 'approve' | 'abort'")),
		mcp.WithString("idempotencyKey"),
	), sendMessageHandler(core))

	s.AddTool(mcp.NewTool("wait_for_messages",
		mcp.WithDescription("Block (up to timeoutMs) until transcript messages appear at or after sinceIndex. You almost never need this — human messages already arrive live as your next input, no polling required. sinceIndex is INCLUSIVE and NOT filtered by `from`, so a raw send_message index returns your own message back to you; always pass the previous response's nextIndex."),
		mcp.WithNumber("sinceIndex"),
		mcp.WithNumber("timeoutMs"),
	), waitForMessagesHandler(core))

	s.AddTool(mcp.NewTool("AskUserQuestion",
		mcp.WithDescription("Ask the human one or more structured multiple-choice questions. Answered via the web dashboard. Blocks (up to timeoutMs) until answered, or returns {\"status\":\"pending\"} if it times out first (call again with the same questions to keep waiting). See docs/adr/0018."),
		mcp.WithInputSchema[AskUserQuestionArgs](),
	), askUserQuestionHandler(core))

	s.AddTool(mcp.NewTool("request_e2e_env",
		mcp.WithDescription("Request this task's e2e environment: a pod running the app on this branch, plus code-server and Playwright. Safe to call again — returns the existing session's URL. The response echoes the recipe actually used (resolvedStartCmd, profileName, tools, services) from the repo's dashboard-editable profile. If the preview does not serve, read resolvedStartCmd and report what is wrong — do not silently substitute your own command. Cold install takes 10+ minutes and the URL won't serve until the app starts; check with run_command 'ps aux'."),
		mcp.WithString("startCmd", mcp.Description("Override the profile's start command, THIS TASK ONLY, never saved. Requires human approval first; if declined or unanswered, the profile's own command is used. Omit unless you have concrete evidence the profile is wrong. Whatever runs must bind 0.0.0.0 on $PORT — a localhost or default-port bind is unreachable from outside the pod and the preview never serves.")),
		mcp.WithString("profile", mcp.Description("Override WHICH of the repo's profiles to build from (e.g. \"lint\"). THIS TASK ONLY, never saved, and requires human approval like startCmd. Omit unless you need a different toolchain than the one you got — the response's profileName/tools tell you what you actually have.")),
	), requestE2eEnvHandler(core, s))

	s.AddTool(mcp.NewTool("kill_env",
		mcp.WithDescription("Permanently destroy this task's e2e environment. Do NOT use it to 'refresh' the app after editing files — the shared worktree hot-reloads, just reload the preview URL. Only when completely done: recreating costs 10+ minutes of cold install."),
	), killEnvHandler(core))

	// Registered statically, unlike the Playwright tools below it: this is
	// the worker's build/test sandbox (docs/adr/0039), so it has to exist
	// from the session's first turn rather than appear only after a
	// request_e2e_env call — which also meant it never came back at all on a
	// resumed session, and depended on notifications/tools/list_changed
	// being honored (a risk docs/adr/0012 flagged and never verified).
	s.AddTool(mcp.NewTool("run_command",
		mcp.WithDescription("Run a shell command in this task's sandbox — a separate pod with the repo's toolchain, its services (Postgres/Redis), a warm dependency cache and the app's dev server, sharing this task's worktree. Prefer this over Bash for anything that builds, tests, lints, installs, or runs the app; it starts the sandbox on first use, so no setup call is needed. Returns stdout, stderr, exitCode — a nonzero exit is information, not an error. The sandbox has NO git and no GitHub credentials on purpose: run git, gh and PR work through Bash. Background long-running processes with a trailing &; timeout 15 minutes. Large output is truncated with the full log's path in fullOutputPath — tail/grep it with this same tool."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command, run via bash -lc from the worktree root")),
	), runCommandHandler(core, s))

	s.AddTool(mcp.NewTool("list_shared_files",
		mcp.WithDescription("List files in the fleet-wide shared file space (a single flat Garage S3 bucket, shared by every task and by the human via the dashboard). Returns each file's key, size, last-modified time, and content type."),
	), listSharedFilesHandler(core))

	s.AddTool(mcp.NewTool("get_shared_file_upload_url",
		mcp.WithDescription("Presigned URL to upload a file into the fleet-wide shared file space. The filename becomes the object key verbatim (flat namespace, last write wins). Moves no bytes — upload from Bash: curl -T <local-path> \"<uploadUrl>\"."),
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
		mcp.WithDescription("Send a message to another session, as yourself. It lands in that session's transcript attributed to you and warms its pod if idle. Use it to hand off work or ask a question of a session owning a different repo — not to chat; the target reads it the way it reads a human's, so be concrete. REFUSED if the target is 'blocked' (it is waiting on a human decision that is not yours to resolve), for your own session, and beyond a small relay depth so chains cannot loop. Follow with wait_for_agent to await a reply."),
		mcp.WithString("taskId", mcp.Required(), mcp.Description("Target session's task id, from list_sessions")),
		mcp.WithString("text", mcp.Required()),
	), promptSessionHandler(core))

	s.AddTool(mcp.NewTool("wait_for_agent",
		mcp.WithDescription("Block until another session reaches a liveness state, or the timeout expires. With no `until`, waits for it to settle (idle, done, blocked, stalled). `until: blocked` is the useful one after prompting — it returns when that session genuinely needs a human. A timeout is not an error: the response reports timedOut plus the state actually reached, and 'still working' is a legitimate answer to act on."),
		mcp.WithString("taskId", mcp.Required()),
		mcp.WithString("until", mcp.Description("working | blocked | idle | done | stalled | unknown. Omit to wait for any settled state.")),
		mcp.WithNumber("timeoutMs", mcp.Description("Default 120000.")),
		mcp.WithNumber("afterSeq", mcp.Description("Only count a settled state once the target produces activity newer than this transcript seq. Filled in automatically from your last prompt_agent to the same target, so normally omit it — without it, waiting right after prompting can return the state held BEFORE your message landed.")),
	), waitForSessionHandler(core))

	s.AddTool(mcp.NewTool("view_logs",
		mcp.WithDescription("View recent logs from fleet components or deployed apps. Use to debug worker, sidecar, or the deployed app during e2e tests. Supports duration-based queries (duration) or explicit ranges (start_time/end_time, RFC3339)."),
		mcp.WithString("component", mcp.Required(), mcp.Description("Which component: worker|sidecar|core|provisioner|e2e|app")),
		mcp.WithString("app_name", mcp.Description("For component=app, the deployed app's name — usually this task's own repo name. Repos are dashboard-managed (docs/adr/0028), so there is no fixed list.")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace (default: agent-fleet, use 'default' for deployed apps)")),
		mcp.WithString("level", mcp.Description("Filter by level: debug|info|warn|error (default: all)")),
		mcp.WithString("duration", mcp.Description("How far back: 1h|30m|6h|24h (default: 1h) - ignored if start_time is set")),
		mcp.WithNumber("limit", mcp.Description("Max entries to return (default 50, max 1000)")),
		mcp.WithString("start_time", mcp.Description("Optional: RFC3339 timestamp (e.g., '2024-01-15T10:30:00Z') - overrides duration")),
		mcp.WithString("end_time", mcp.Description("Optional: RFC3339 timestamp (default: now)")),
	), viewLogsHandler(core))

	// Browser tools, same static treatment as run_command above and for the
	// same reason (docs/adr/0044). They proxy to a pod that may not exist yet;
	// that is fine and deliberate — the call fails loudly if nothing is behind
	// it, which beats the tool being invisible and the agent concluding it has
	// no browser.
	registerPlaywrightTools(s, core)

	return server.NewStreamableHTTPServer(s)
}

// Per-result caps for the two tools here that can return arbitrarily much
// (ADR-0046). Neither is bounded by anything upstream: view_logs' limit is
// an entry count, not a byte count, and a structured log line has no length
// bound at all; wait_for_messages returns whatever text a human or another
// session wrote.
//
// This matters more here than it would in a normal MCP server because
// nothing downstream ever sheds these bytes again: Claude Code's
// microcompaction operates on a hardcoded built-in tool set and never
// touches MCP results, so every one of these stays in the session verbatim
// until auto-compact summarizes the entire conversation.
const (
	maxLogsBytes         = 15000
	maxMessageEntryBytes = 2000
)

// truncate cuts s to max bytes and appends note, which must tell the caller
// how to get the rest. Claude Code's own MCP truncation notice reads "If
// this MCP server provides pagination or filtering tools, use them to
// retrieve specific portions of the data" — that instruction is addressed
// to this function. A cap without a stated way back leaves the agent
// working from partial data without knowing it, which is a worse failure
// than a large context.
func truncate(s string, max int, note string) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s\n\n[OUTPUT TRUNCATED - first %d of %d bytes shown] %s", s[:max], max, len(s), note)
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
			// Per entry, not per batch: one pasted stack trace shouldn't
			// crowd out the five short directives that came after it.
			text := truncate(e.GetText(), maxMessageEntryBytes,
				"This one message was long; the rest of the batch is intact. Ask the sender to re-send the specific part you need.")
			messages = append(messages, map[string]any{"from": e.GetFrom(), "text": text, "type": protoTypeToString(e.GetType())})
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

	// The profile-name override (docs/adr/0044) reuses the same gate rather
	// than growing a second approval mechanism: ADR-0034's rule is that a
	// task branch must never silently change what the provisioner builds, and
	// which profile is used is a strictly bigger lever than which command it
	// runs — it decides the toolchain and the services too.
	profileOverrideQuestion = "The agent wants to build this task's e2e environment from a different profile than the repo's configured one. Which should be used?"
	profileKeepConfigured   = "Use the configured profile"
	profileUseOverride      = "Use the agent's"
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
	return confirmOverride(ctx, core, overrideQuestion{
		question:    startCmdOverrideQuestion,
		header:      "e2e cmd",
		keepLabel:   startCmdKeepProfile,
		keepDesc:    "Run the repo's configured e2e profile command, as edited in the dashboard. Ignores the agent's proposal.",
		acceptLabel: startCmdUseOverride,
		acceptDesc:  "Run this instead, for this task only (the profile is not modified): " + startCmd,
	})
}

// confirmProfileOverride is the same gate for the profile-name override
// (docs/adr/0044) — the agent picking a different recipe entirely.
func confirmProfileOverride(ctx context.Context, core questionAsker, profile string) bool {
	return confirmOverride(ctx, core, overrideQuestion{
		question:    profileOverrideQuestion,
		header:      "e2e profile",
		keepLabel:   profileKeepConfigured,
		keepDesc:    "Build the environment from the profile configured for this repo in the dashboard. Ignores the agent's proposal.",
		acceptLabel: profileUseOverride,
		acceptDesc:  "Build it from this profile instead, for this task only (nothing is saved): " + profile,
	})
}

type overrideQuestion struct {
	question, header        string
	keepLabel, keepDesc     string
	acceptLabel, acceptDesc string
}

// confirmOverride blocks on a real human answer before an agent's proposal is
// allowed to beat the repo's configuration. Anything other than an explicit
// pick of the override — decline, timeout, malformed answer — means the
// configuration wins, matching docs/adr/0029's rule that a decision is never
// inferred from silence. Enforced here rather than left to the agent to ask:
// an agent choosing whether to check is exactly how a guessed command
// silently replaced a correct profile and 502'd the preview.
func confirmOverride(ctx context.Context, core questionAsker, q overrideQuestion) bool {
	payload, err := json.Marshal(map[string]any{"questions": []AskUserQuestionQuestion{{
		Question: q.question,
		Header:   q.header,
		Options: []AskUserQuestionOption{
			{Label: q.keepLabel, Description: q.keepDesc},
			{Label: q.acceptLabel, Description: q.acceptDesc},
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
	return parsed.Answers[q.question] == q.acceptLabel
}

func requestE2eEnvHandler(core *coreclient.Client, s *server.MCPServer) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		startCmd := req.GetString("startCmd", "")
		profile := req.GetString("profile", "")
		slog.Info("mcp request_e2e_env",
			"startCmdOverrideProposed", startCmd != "", "profileOverrideProposed", profile != "")
		if startCmd != "" && !confirmStartCmdOverride(ctx, core, startCmd) {
			startCmd = ""
		}
		if profile != "" && !confirmProfileOverride(ctx, core, profile) {
			profile = ""
		}
		resp, err := core.RequestE2eEnv(ctx, startCmd, profile)
		if err != nil {
			slog.Error("mcp request_e2e_env", "error", err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Nothing to register here anymore: the Playwright tools are static
		// (New() above, docs/adr/0044). This used to discover them live and
		// fire notifications/tools/list_changed, which could not work — see
		// registerPlaywrightTools for the three ways it missed.

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

// runCommandRetryDelays paces the wait for a freshly provisioned sandbox to
// become reachable: ~65s total, which covers a normal image pull without
// making a genuinely broken pod cost the agent minutes before it hears about
// it. A var, not a const array, so tests can zero it.
var runCommandRetryDelays = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	30 * time.Second,
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
	RequestE2eEnv(ctx context.Context, startCmd, profile string) (*agentfleetv1.RequestE2EEnvResponse, error)
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
		// live sandbox pod. Start one and wait for it to come up.
		slog.Info("mcp run_command: no live sandbox, provisioning one", "error", err)

		// This used to be one provision plus one immediate retry, which could
		// essentially never succeed: RequestE2eEnv returns as soon as the pod
		// OBJECT exists (Pending — scheduling, image pull, init containers all
		// still ahead of it), so a zero-delay retry always hit an unreachable
		// exec listener and the agent was told the sandbox was broken on the
		// very first command of most sessions (docs/adr/0044).
		//
		// RequestE2eEnv is idempotent, so re-calling it each round is both the
		// wait and the status probe: env.Status carries the pod's real phase,
		// which is what makes the final error message diagnostic rather than
		// just "not reachable".
		var env *agentfleetv1.RequestE2EEnvResponse
		var provErr error
		for i, delay := range runCommandRetryDelays {
			env, provErr = core.RequestE2eEnv(ctx, "", "")
			if provErr != nil {
				slog.Error("mcp run_command: provisioning failed", "error", provErr)
				return mcp.NewToolResultError(fmt.Sprintf("run_command: no sandbox running and one could not be started: %v", provErr)), nil
			}

			resultJSON, isError, err = core.CallE2eTool(ctx, runCommandToolName, string(argsJSON))
			if err == nil {
				break
			}
			slog.Info("mcp run_command: sandbox not reachable yet, waiting",
				"attempt", i+1, "of", len(runCommandRetryDelays), "podStatus", env.GetStatus(), "error", err)
			select {
			case <-ctx.Done():
				return mcp.NewToolResultError(fmt.Sprintf("run_command: gave up waiting for the sandbox: %v", ctx.Err())), nil
			case <-time.After(delay):
			}
		}
		if err != nil {
			slog.Error("mcp run_command: still unreachable after provisioning", "error", err, "podStatus", env.GetStatus())
			return mcp.NewToolResultError(fmt.Sprintf(
				"run_command: the sandbox pod is %q and its exec listener is still unreachable after %d attempts: %v\n"+
					"resolvedStartCmd: %q (profile %q)\n"+
					"A pod stuck at \"requested\" is usually still pulling the image or installing dependencies — try again shortly. "+
					"A %q or %q pod is broken: call kill_env and then run_command again to rebuild it.",
				env.GetStatus(), len(runCommandRetryDelays), err,
				env.GetResolvedStartCmd(), env.GetProfileName(), "failed", "unknown")), nil
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

// playwrightToolsJSON is a snapshot of @playwright/mcp's tools/list, taken
// from the e2e-runner image. Embedded rather than discovered at runtime —
// see registerPlaywrightTools.
//
// To refresh after bumping @playwright/mcp, run a container off
// e2e-runner/Dockerfile, POST tools/list to its :8931, and write the sorted
// `result.tools` array here. Drift is a degradation, never a break: a tool
// that disappeared upstream fails loudly at the real server on first call, a
// new one is merely invisible until this file is refreshed.
//
//go:embed playwright_tools.json
var playwrightToolsJSON []byte

// playwrightTool is the subset of an MCP tool descriptor the snapshot carries.
// InputSchema stays raw so it round-trips whatever @playwright/mcp declares,
// without this package needing to model JSON Schema.
type playwrightTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// registerPlaywrightTools registers the browser tools statically, at startup,
// exactly as docs/adr/0039 did for run_command and for the same reason: a
// tool the agent cannot see is a tool that does not exist.
//
// These used to be discovered live and registered after request_e2e_env, then
// again from run_command's provisioning branch. That could not work, three
// ways over (docs/adr/0044):
//
//  1. Both call sites fire immediately after RequestE2eEnv returns — which is
//     when the pod object exists, not when it serves. Measured on the real
//     image: execmcp binds at t+2s, @playwright/mcp at t+5s. The discovery
//     call lands in that gap and gets connection-refused.
//  2. run_command breaks out of its retry loop the moment execmcp answers, so
//     the later attempts that might have caught :8931 never happen.
//  3. On a resumed session with a warm pod, run_command's very first call
//     succeeds, so the provisioning branch — the only thing that registered
//     anything — is never reached at all.
//
// And all three were moot anyway, because @playwright/mcp answered every
// provisioner request with 403 until --allowed-hosts landed. ProxiedTools
// swallows any of that into a nil list at log level Info, so the whole thing
// failed silently for the agent's entire session.
//
// Static registration also drops the dependency on the client honoring
// notifications/tools/list_changed, which docs/adr/0012 flagged as a risk and
// nobody ever verified.
func registerPlaywrightTools(s *server.MCPServer, core *coreclient.Client) {
	var tools []playwrightTool
	if err := json.Unmarshal(playwrightToolsJSON, &tools); err != nil {
		// Embedded and covered by a test, so this is a build-time bug, not a
		// runtime condition — but a sidecar that starts without browser tools
		// must say so rather than look normal.
		slog.Error("mcp: embedded Playwright tool snapshot is unreadable — browser tools unavailable", "error", err)
		return
	}
	for _, t := range tools {
		if t.Name == runCommandToolName {
			continue // never shadow New()'s pod-provisioning handler
		}
		addProxiedTool(s, core, t)
	}
	slog.Info("mcp: registered Playwright tools", "count", len(tools))
}

// addProxiedTool registers one Playwright tool as a straight passthrough to
// the e2e pod, via core (docs/adr/0020's hub-and-spoke rule). The pod need
// not exist yet: the tool is advertised from turn one and the call fails
// loudly if there's nothing behind it, which is strictly better than the tool
// being absent and the agent concluding it cannot use a browser at all.
func addProxiedTool(s *server.MCPServer, core *coreclient.Client, desc playwrightTool) {
	var schema mcp.ToolInputSchema
	_ = json.Unmarshal(desc.InputSchema, &schema)
	tool := mcp.Tool{Name: desc.Name, Description: desc.Description, InputSchema: schema}
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argsJSON, err := json.Marshal(req.GetArguments())
		if err != nil {
			return nil, fmt.Errorf("marshal tool arguments: %w", err)
		}
		resultJSON, isError, err := core.CallE2eTool(ctx, desc.Name, string(argsJSON))
		if err != nil {
			return nil, err
		}
		return e2eToolResult(resultJSON, isError, "")
	})
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

		// view_logs already has the filtering tools the truncation notice
		// should point at — limit, level, duration, start_time/end_time. No
		// offset cursor is added here on purpose: narrowing the window or
		// the level is how you actually page logs, and a cursor would mean a
		// proto change plus a core change to buy nothing the existing
		// parameters don't already do.
		return mcp.NewToolResultText(truncate(logsText, maxLogsBytes,
			"Narrow the query rather than re-running it as-is: lower `limit`, raise `level`, shorten `duration`, or set `start_time`/`end_time` around the window you care about.")), nil
	}
}
