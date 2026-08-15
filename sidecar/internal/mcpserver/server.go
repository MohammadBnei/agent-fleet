// Package mcpserver hosts the sidecar's local (localhost-only) MCP server —
// the Agent SDK session's mcpServers config points here, never at core
// directly. Every tool proxies onward to core over the sidecar's one outbound
// gRPC connection.
//
// It is purely local again, as docs/adr/0020 point 6 originally required.
// docs/adr/0045 carved out one exception — the sandbox tools dialled their
// task's e2e pod over MCP — and docs/adr/0048 §6 removes it by removing the
// sandbox: there is no second pod to dial. The carve-out is not reversed on
// its merits; it simply has nothing left to apply to.
//
// One sidecar only ever serves the one session its pod was created for, which
// is why no tool here takes a session id.
package mcpserver

import (
	"context"
	_ "embed"
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

// New builds the sidecar's local MCP HTTP handler. Every tool the agent will
// ever see is registered here, at startup. Nothing is discovered at runtime
// and nothing depends on notifications/tools/list_changed; a tool the agent
// cannot see is a tool that does not exist, and lazy registration lost that
// race three different ways (docs/adr/0044).
//
// The e2e directDialer parameter is gone with the sandbox it dialed
// (docs/adr/0048 §6) — this sidecar now talks only to core.
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

	// request_e2e_env, kill_env and run_command are gone (docs/adr/0048 §6).
	//
	// All three existed to operate a second pod, and the second pod existed so
	// that ONE tool could skip a permission prompt. The agent's app now runs in
	// this same pod, started with plain Bash, so there is no environment to
	// request, nothing to kill, and no command to forward: `bun install` and
	// `go test` are Bash calls, un-prompted via allow-rules in
	// fleet-shared/settings.json — the same file a CLI user edits — rather than
	// via which pod the command happened to land in.
	//
	// What is left is the two things the agent genuinely cannot do for itself,
	// because both need cluster RBAC this pod deliberately does not have. Both
	// route agent -> sidecar -> core -> provisioner.
	s.AddTool(mcp.NewTool("expose",
		mcp.WithDescription("Publish a port from this pod at a public HTTPS URL, and return it. Start your server first with Bash (it must bind 0.0.0.0 on that port — a localhost bind is unreachable from outside the pod), then call this. Safe to call again: re-exposing the same port returns the same URL. The URL dies with this pod, so a stopped or idle-timed-out session's preview stops serving; call expose again after a warm."),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("Port your server is listening on inside this pod")),
	), exposeHandler(core))

	s.AddTool(mcp.NewTool("unexpose",
		mcp.WithDescription("Stop publishing this session's port. Removes the Service and route; your server keeps running. Teardown does this automatically, so only call it if you want the URL gone while the session continues."),
	), unexposeHandler(core))

	s.AddTool(mcp.NewTool("request_service",
		mcp.WithDescription("Provision (or reuse) a shared backing service and return a connection string. Instances are shared per repo, not per session, so another session on this repo may already be using it — treat the database as shared and namespace what you create. This is the one thing you cannot start yourself with Bash, because it needs cluster permissions this pod does not have."),
		mcp.WithString("kind", mcp.Required(), mcp.Description("Service to provision: \"postgres\" or \"redis\"")),
	), requestServiceHandler(core))

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
		mcp.WithString("sessionId", mcp.Required(), mcp.Description("Target session's task id, from list_sessions")),
		mcp.WithString("text", mcp.Required()),
	), promptSessionHandler(core))

	s.AddTool(mcp.NewTool("wait_for_agent",
		mcp.WithDescription("Block until another session reaches a liveness state, or the timeout expires. With no `until`, waits for it to settle (idle, done, blocked, stalled). `until: blocked` is the useful one after prompting — it returns when that session genuinely needs a human. A timeout is not an error: the response reports timedOut plus the state actually reached, and 'still working' is a legitimate answer to act on."),
		mcp.WithString("sessionId", mcp.Required()),
		mcp.WithString("until", mcp.Description("working | blocked | idle | done | stalled | unknown. Omit to wait for any settled state.")),
		mcp.WithNumber("timeoutMs", mcp.Description("Default 120000.")),
		mcp.WithNumber("afterSeq", mcp.Description("Only count a settled state once the target produces activity newer than this transcript seq. Filled in automatically from your last prompt_agent to the same target, so normally omit it — without it, waiting right after prompting can return the state held BEFORE your message landed.")),
	), waitForSessionHandler(core))

	s.AddTool(mcp.NewTool("view_logs",
		mcp.WithDescription("View recent logs from fleet components or deployed apps. Supports duration-based queries (duration) or explicit ranges (start_time/end_time, RFC3339). Your own session's logs are the \"worker\" component — including whatever your app writes to stdout, since it runs in this same pod."),
		mcp.WithString("component", mcp.Required(), mcp.Description("Which component: worker|sidecar|core|provisioner|app")),
		mcp.WithString("app_name", mcp.Description("For component=app, the deployed app's name — usually this task's own repo name. Repos are dashboard-managed (docs/adr/0028), so there is no fixed list.")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace (default: agent-fleet, use 'default' for deployed apps)")),
		mcp.WithString("level", mcp.Description("Filter by level: debug|info|warn|error (default: all)")),
		mcp.WithString("duration", mcp.Description("How far back: 1h|30m|6h|24h (default: 1h) - ignored if start_time is set")),
		mcp.WithNumber("limit", mcp.Description("Max entries to return (default 50, max 1000)")),
		mcp.WithString("start_time", mcp.Description("Optional: RFC3339 timestamp (e.g., '2024-01-15T10:30:00Z') - overrides duration")),
		mcp.WithString("end_time", mcp.Description("Optional: RFC3339 timestamp (default: now)")),
	), viewLogsHandler(core))

	// The proxied Playwright tools are gone with the pod they proxied to.
	// Playwright now runs in this pod on localhost, registered by the worker as
	// a second `mcpServers` entry in its query() call — so the agent talks to
	// the real server directly.
	//
	// That deletes the entire class of failure docs/adr/0044 documented: no
	// embedded tool-list snapshot to drift from the installed version, no
	// registration racing a pod that is still starting, and no proxy hop to
	// swallow an error into an empty result. It also deletes the reason the
	// snapshot existed — a tool the agent cannot see is a tool that does not
	// exist, and the SDK handles a locally-configured MCP server's tool list
	// itself.
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
		answered, answersJSON, _, err := core.AskUserQuestion(ctx, string(payload), timeoutMs)
		if err != nil {
			slog.Error("mcp AskUserQuestion", "error", err)
			return nil, fmt.Errorf("AskUserQuestion: %w", err)
		}
		if answered {
			return mcp.NewToolResultText(answersJSON), nil
		}
		body, _ := json.Marshal(map[string]string{"status": "pending"})
		return mcp.NewToolResultText(string(body)), nil
	})
}

// exposeHandler publishes a port from this pod at a public HTTPS URL.
//
// The agent starts its own server with Bash and then asks for a URL, rather
// than the fleet knowing how to start the app. That is the whole of what
// replaced the recipe system (docs/adr/0048 §6): three tables, an override
// approval gate and an ingredient-env mechanism existed so the platform could
// answer "how do I run this repo" — a question `cat package.json` answers, for
// a reader who is already sitting in the repo.
//
// Routed through core to the provisioner because creating a Service and an
// IngressRoute needs cluster RBAC, and this pod holds none by design.
func exposeHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		port := req.GetInt("port", 0)
		if port <= 0 || port > 65535 {
			return mcp.NewToolResultError("port must be between 1 and 65535"), nil
		}
		slog.Info("mcp expose", "port", port)
		url, err := core.Expose(ctx, int32(port))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("expose: %v", err)), nil
		}
		// The URL alone, with the caveat that made docs/adr/0044 an incident:
		// a route existing is not a server answering. If the agent's process
		// is not actually listening on 0.0.0.0, this returns a URL that 502s,
		// and only the agent can tell the difference.
		body, _ := json.Marshal(map[string]string{
			"url":  url,
			"note": "The route exists; that is not the same as your server answering. Check it yourself before reporting it as working.",
		})
		return mcp.NewToolResultText(string(body)), nil
	}
}

func unexposeHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slog.Info("mcp unexpose")
		if err := core.Unexpose(ctx); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("unexpose: %v", err)), nil
		}
		return mcp.NewToolResultText("unexposed"), nil
	}
}

// requestServiceHandler provisions or reuses a shared Postgres/Redis.
//
// The one capability that stays fleet-side after the sandbox merge, and it
// stays for a specific reason rather than by omission: it needs cluster RBAC.
// Everything else the recipe system did was the fleet reading the repo on the
// agent's behalf, which the agent can do better because it is already there.
func requestServiceHandler(core *coreclient.Client) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kind := req.GetString("kind", "")
		if kind != "postgres" && kind != "redis" {
			return mcp.NewToolResultError(`kind must be "postgres" or "redis"`), nil
		}
		slog.Info("mcp request_service", "kind", kind)
		dsn, err := core.RequestService(ctx, kind)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("request_service: %v", err)), nil
		}
		body, _ := json.Marshal(map[string]string{
			"kind": kind,
			"dsn":  dsn,
			"note": "Shared per repo, not per session — another session on this repo may be using it too.",
		})
		return mcp.NewToolResultText(string(body)), nil
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
