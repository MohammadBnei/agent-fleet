// Package coreclient is the sidecar's one outbound connection — the single
// upstream channel both the local MCP server (agent-facing) and the local
// plain API (wrapper-facing) funnel through (docs/adr/0020 point 5). A
// sidecar instance only ever serves one task (one pod = one task,
// docs/adr/0019), so every method here is scoped to that task implicitly —
// callers don't pass a taskID, the Client already has it.
package coreclient

import (
	"context"
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type Client struct {
	conn   *grpc.ClientConn
	rpc    agentfleetv1.CoreServiceClient
	taskID string
	ready  atomic.Bool
}

// retryServiceConfig bounds-retries transient "produced zero addresses"
// blips (core is a single replica — any restart briefly empties the
// Service's DNS answer) without hanging forever if core is actually down
// for longer than that. Covers both codes the resolver failure has been
// observed to surface as: Unavailable once a connection exists, Canceled
// when it fails before one's ever established (confirmed live).
const retryServiceConfig = `{
	"methodConfig": [{
		"name": [{"service": "agentfleet.v1.CoreService"}],
		"waitForReady": true,
		"retryPolicy": {
			"MaxAttempts": 5,
			"InitialBackoff": "0.5s",
			"MaxBackoff": "5s",
			"BackoffMultiplier": 2.0,
			"RetryableStatusCodes": ["UNAVAILABLE", "CANCELLED"]
		}
	}]
}`

func New(addr, taskID string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(retryServiceConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("dial core: %w", err)
	}
	return &Client{
		conn:   conn,
		rpc:    agentfleetv1.NewCoreServiceClient(conn),
		taskID: taskID,
	}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Ready reports whether WaitReady has ever observed a live connection to
// core. Backs the sidecar's /readyz endpoint, which gates the provisioner's
// StartupProbe on core actually being reachable — not just this process
// being up (grpc.NewClient's dial is lazy, so a bare TCP/process check
// proves nothing about core connectivity).
func (c *Client) Ready() bool {
	return c.ready.Load()
}

// WaitReady blocks until core.conn reaches connectivity.Ready (retrying
// through DNS/dial failures like "produced zero addresses" indefinitely,
// bounded only by ctx) or ctx is done. Uses grpc's own connection-state API
// rather than a synthetic ping RPC — no server-side call, no journal/log
// side effects from the readiness probe itself.
func (c *Client) WaitReady(ctx context.Context) error {
	c.conn.Connect()
	for {
		state := c.conn.GetState()
		if state == connectivity.Ready {
			c.ready.Store(true)
			return nil
		}
		if !c.conn.WaitForStateChange(ctx, state) {
			return ctx.Err()
		}
	}
}

// --- agent-facing (proxied by the local MCP server) ---

func (c *Client) SendMessage(ctx context.Context, from, text string, msgType agentfleetv1.TranscriptEntryType, idempotencyKey string) (int64, error) {
	resp, err := c.rpc.SendMessage(ctx, &agentfleetv1.SendMessageRequest{
		SessionId: c.taskID, From: from, Text: text, Type: msgType, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return 0, fmt.Errorf("SendMessage: %w", err)
	}
	return resp.GetSeq(), nil
}

// AppendReplyMessage is SendMessage plus reply-to correlation — the same
// two-method split transcript.Store itself uses, so the many callers that
// don't need correlation aren't forced to pass a meaningless seq.
func (c *Client) AppendReplyMessage(ctx context.Context, from, text string, msgType agentfleetv1.TranscriptEntryType, idempotencyKey string, replyToSeq int64) (int64, error) {
	resp, err := c.rpc.SendMessage(ctx, &agentfleetv1.SendMessageRequest{
		SessionId: c.taskID, From: from, Text: text, Type: msgType,
		IdempotencyKey: idempotencyKey, ReplyToSeq: &replyToSeq,
	})
	if err != nil {
		return 0, fmt.Errorf("AppendReplyMessage: %w", err)
	}
	return resp.GetSeq(), nil
}

func (c *Client) WaitForMessages(ctx context.Context, sinceSeq int64, timeoutMs int32) ([]*agentfleetv1.TranscriptEntry, int64, error) {
	resp, err := c.rpc.WaitForMessages(ctx, &agentfleetv1.ReadTranscriptSinceRequest{
		SessionId: c.taskID, SinceSeq: sinceSeq, TimeoutMs: timeoutMs,
	})
	if err != nil {
		return nil, sinceSeq, fmt.Errorf("WaitForMessages: %w", err)
	}
	return resp.GetEntries(), resp.GetNextSeq(), nil
}

func (c *Client) AskUserQuestion(ctx context.Context, questionsJSON string, timeoutMs int32) (answered bool, answersJSON string, questionSeq int64, err error) {
	resp, err := c.rpc.AskUserQuestion(ctx, &agentfleetv1.AskUserQuestionRequest{
		SessionId: c.taskID, QuestionsJson: questionsJSON, TimeoutMs: timeoutMs,
	})
	if err != nil {
		return false, "", 0, fmt.Errorf("AskUserQuestion: %w", err)
	}
	return resp.GetAnswered(), resp.GetAnswersJson(), resp.GetQuestionSeq(), nil
}

// RequestE2eEnv and KillE2eEnv are gone with the sandbox (docs/adr/0048 §6).
// Both carried the recipe system on their signatures — startCmd, profile, and
// a whole resolved-recipe response the agent was meant to read back. None of
// it survives, because none of it was ever a question the fleet was better
// placed to answer than the agent sitting in the repo.

// Expose publishes a port from this pod and returns its public URL.
func (c *Client) Expose(ctx context.Context, port int32) (string, error) {
	resp, err := c.rpc.Expose(ctx, &agentfleetv1.ExposeRequest{SessionId: c.taskID, Port: port})
	if err != nil {
		return "", fmt.Errorf("Expose: %w", err)
	}
	return resp.GetUrl(), nil
}

func (c *Client) Unexpose(ctx context.Context) error {
	if _, err := c.rpc.Unexpose(ctx, &agentfleetv1.UnexposeRequest{SessionId: c.taskID}); err != nil {
		return fmt.Errorf("Unexpose: %w", err)
	}
	return nil
}

// RequestService provisions or reuses a shared Postgres/Redis and returns its
// connection string. The repo is not a parameter: core owns the session row,
// so it resolves which repo this session is on rather than trusting a pod to
// say — the same reason every other call here is keyed by taskID alone.
func (c *Client) RequestService(ctx context.Context, kind string) (string, error) {
	resp, err := c.rpc.RequestService(ctx, &agentfleetv1.RequestServiceRequest{SessionId: c.taskID, Kind: kind})
	if err != nil {
		return "", fmt.Errorf("RequestService: %w", err)
	}
	return resp.GetDsn(), nil
}

// No sandbox tool calls here at all any more (docs/adr/0045). Tool discovery
// went first (docs/adr/0044 — a static embedded snapshot in mcpserver, because
// discovery always lost the race against the pod becoming reachable), and now
// the calls themselves: the sidecar dials its own task's sandbox over MCP,
// from the ServiceEndpoint roster, so core is no longer in the path of a shell
// command. What survives on this client is what core alone can do — the
// transcript, the journal, questions, and commanding a sandbox into existence.

// ListFiles, GetFileUploadURL, GetFileDownloadURL, and DeleteFile back the
// shared file space (docs/adr/0030) — the flat namespace isn't scoped to
// this sidecar's own task, so unlike every other agent-facing method here
// these don't pass c.taskID.

func (c *Client) ListFiles(ctx context.Context) ([]*agentfleetv1.FileMetadata, error) {
	resp, err := c.rpc.ListFiles(ctx, &agentfleetv1.ListFilesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListFiles: %w", err)
	}
	return resp.GetFiles(), nil
}

func (c *Client) GetFileUploadURL(ctx context.Context, filename, contentType string) (uploadURL, key, expiresAt string, err error) {
	resp, err := c.rpc.GetFileUploadUrl(ctx, &agentfleetv1.GetFileUploadUrlRequest{Filename: filename, ContentType: contentType})
	if err != nil {
		return "", "", "", fmt.Errorf("GetFileUploadUrl: %w", err)
	}
	return resp.GetUploadUrl(), resp.GetKey(), resp.GetExpiresAt(), nil
}

func (c *Client) GetFileDownloadURL(ctx context.Context, key string) (downloadURL, expiresAt string, err error) {
	resp, err := c.rpc.GetFileDownloadUrl(ctx, &agentfleetv1.GetFileDownloadUrlRequest{Key: key})
	if err != nil {
		return "", "", fmt.Errorf("GetFileDownloadUrl: %w", err)
	}
	return resp.GetDownloadUrl(), resp.GetExpiresAt(), nil
}

func (c *Client) DeleteFile(ctx context.Context, key string) error {
	_, err := c.rpc.DeleteFile(ctx, &agentfleetv1.DeleteFileRequest{Key: key})
	if err != nil {
		return fmt.Errorf("DeleteFile: %w", err)
	}
	return nil
}

// --- wrapper-facing (proxied by the local plain API) ---

// Heartbeat and SetTaskStatus used to live here. Both are deleted in
// docs/adr/0048: liveness reconciles against Kubernetes rather than a timer
// inside the worker, and a polymorphic session has no completion the worker
// is in a position to report.

func (c *Client) AppendJournal(ctx context.Context, repo, actor, eventType, payloadJSON string) error {
	_, err := c.rpc.AppendJournal(ctx, &agentfleetv1.AppendJournalRequest{
		Repo: repo, Actor: actor, EventType: eventType, PayloadJson: payloadJSON,
	})
	if err != nil {
		return fmt.Errorf("AppendJournal: %w", err)
	}
	return nil
}

func (c *Client) SearchJournal(ctx context.Context, repo, query string, limit int32) ([]*agentfleetv1.JournalEntry, error) {
	resp, err := c.rpc.SearchJournal(ctx, &agentfleetv1.SearchJournalRequest{
		Repo: repo, Query: query, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("SearchJournal: %w", err)
	}
	return resp.GetEntries(), nil
}

func (c *Client) SaveSessionID(ctx context.Context, sessionID, model, leaseID string) error {
	_, err := c.rpc.SaveAgentSessionId(ctx, &agentfleetv1.SaveAgentSessionIdRequest{
		SessionId: c.taskID, AgentSessionId: sessionID, Model: model, LeaseId: leaseID,
	})
	if err != nil {
		return fmt.Errorf("SaveSessionId: %w", err)
	}
	return nil
}

// StillHoldsLease is gone with the reclaim that created the race it guarded
// (docs/adr/0048). It had no production caller here either way.

func (c *Client) PushToolTelemetry(ctx context.Context, summaryJSON string) error {
	_, err := c.rpc.PushToolTelemetry(ctx, &agentfleetv1.PushToolTelemetryRequest{SessionId: c.taskID, SummaryJson: summaryJSON})
	if err != nil {
		return fmt.Errorf("PushToolTelemetry: %w", err)
	}
	return nil
}

// StreamHumanMessages opens the live feed and calls onEntry for each new
// human message — the mechanism that lets the wrapper feed streamInput()
// live (docs/adr/0021 point 2). Blocks until the stream ends or ctx is
// cancelled; callers run it in its own goroutine.
func (c *Client) StreamHumanMessages(ctx context.Context, sinceSeq int64, onEntry func(*agentfleetv1.TranscriptEntry)) error {
	stream, err := c.rpc.StreamHumanMessages(ctx, &agentfleetv1.StreamHumanMessagesRequest{SessionId: c.taskID, SinceSeq: sinceSeq})
	if err != nil {
		return fmt.Errorf("StreamHumanMessages: open: %w", err)
	}
	for {
		entry, err := stream.Recv()
		if err != nil {
			return err // io.EOF on clean close, or ctx cancellation — caller decides how to treat it
		}
		onEntry(entry)
	}
}

// ViewLogs queries Loki for recent logs from fleet components or deployed
// apps. Used by the view_logs MCP tool to help agents debug issues by viewing
// logs from worker, sidecar, or deployed applications during e2e tests.
// Supports both duration-based queries and explicit RFC3339 timestamp ranges.
// Returns formatted log text suitable for agent consumption.
func (c *Client) ViewLogs(ctx context.Context, component, appName, namespace, level, duration string, limit int32, startTime, endTime string) (string, error) {
	resp, err := c.rpc.ViewLogs(ctx, &agentfleetv1.ViewLogsRequest{
		Component: component,
		AppName:   appName,
		Namespace: namespace,
		Level:     level,
		Duration:  duration,
		Limit:     limit,
		StartTime: startTime,
		EndTime:   endTime,
	})
	if err != nil {
		return "", fmt.Errorf("ViewLogs: %w", err)
	}
	return resp.GetLogsText(), nil
}

// GetTask fetches the task details from the database, including model and
// permission_mode. Used by the worker on startup to fetch fresh task data
// instead of relying on stale environment variables. CoreService.GetTask,
// not DashboardService.GetTask — DashboardService is a ConnectRPC handler
// mounted only on core's HTTP port, guarded by a same-origin CSRF header
// only the dashboard SPA can set (core/internal/dashboard/interceptor.go);
// it was never reachable over this gRPC connection at all, let alone by a
// non-browser caller.
func (c *Client) GetTask(ctx context.Context) (*agentfleetv1.Session, error) {
	resp, err := c.rpc.GetSession(ctx, &agentfleetv1.GetSessionRequest{Id: c.taskID})
	if err != nil {
		return nil, fmt.Errorf("GetTask: %w", err)
	}
	return resp.GetSession(), nil
}

// SetPermissionMode persists the current permission mode to the database.
// Used by the worker to save the initial "default" mode or when the mode is
// changed via the dashboard. CoreService.SetPermissionMode — see GetTask's
// comment above on why this isn't DashboardService.
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	_, err := c.rpc.SetPermissionMode(ctx, &agentfleetv1.SetPermissionModeRequest{
		SessionId: c.taskID,
		Mode:      mode,
	})
	if err != nil {
		return fmt.Errorf("SetPermissionMode: %w", err)
	}
	return nil
}

// --- inter-agent coordination (docs/adr/0041) ---
//
// The caller's own task id is injected here rather than accepted from the
// agent: it is this pod's identity, not an argument, and letting a prompt
// claim to come from another session would defeat both the self-prompt
// guard and the attribution in the delivered message.

func (c *Client) ListPeerSessions(ctx context.Context) ([]*agentfleetv1.SessionSummary, error) {
	resp, err := c.rpc.ListPeerSessions(ctx, &agentfleetv1.ListPeerSessionsRequest{CallerSessionId: c.taskID})
	if err != nil {
		return nil, fmt.Errorf("ListSessions: %w", err)
	}
	return resp.GetSessions(), nil
}

func (c *Client) PromptSession(ctx context.Context, targetTaskID, text string, depth int32) (*agentfleetv1.PromptSessionResponse, error) {
	resp, err := c.rpc.PromptSession(ctx, &agentfleetv1.PromptSessionRequest{
		CallerSessionId: c.taskID, TargetSessionId: targetTaskID, Text: text, Depth: depth,
	})
	if err != nil {
		return nil, fmt.Errorf("PromptSession: %w", err)
	}
	return resp, nil
}

func (c *Client) WaitForSessionState(ctx context.Context, targetTaskID string, until []string, timeoutMs int32, afterSeq int64) (*agentfleetv1.WaitForSessionStateResponse, error) {
	resp, err := c.rpc.WaitForSessionState(ctx, &agentfleetv1.WaitForSessionStateRequest{
		TargetSessionId: targetTaskID, Until: until, TimeoutMs: timeoutMs, AfterSeq: afterSeq,
	})
	if err != nil {
		return nil, fmt.Errorf("WaitForSessionState: %w", err)
	}
	return resp, nil
}
