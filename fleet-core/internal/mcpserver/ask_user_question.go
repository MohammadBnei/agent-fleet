package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

// AskUserQuestionOption is one choice within an AskUserQuestionQuestion —
// same shape as Claude Code's native AskUserQuestion tool, so a skill's own
// instructions (e.g. architecture-interview's "one question at a time,
// your hypothesis attached") produce well-formed calls without translation.
type AskUserQuestionOption struct {
	Label       string `json:"label" jsonschema_description:"Short label for this option (1-5 words)"`
	Description string `json:"description" jsonschema_description:"What this option means or its tradeoffs"`
}

// AskUserQuestionQuestion is one question in a batch.
type AskUserQuestionQuestion struct {
	Question    string                  `json:"question" jsonschema_description:"The complete question to ask, ending with a question mark"`
	Header      string                  `json:"header" jsonschema_description:"Very short label shown as a chip/tag, max 12 chars"`
	Options     []AskUserQuestionOption `json:"options" jsonschema_description:"2-4 mutually exclusive choices"`
	MultiSelect bool                    `json:"multiSelect,omitempty" jsonschema_description:"Set true to allow selecting more than one option"`
}

// AskUserQuestionArgs is this tool's full input schema, reflected into JSON
// Schema via mcp.WithInputSchema — see docs/adr/0018.
type AskUserQuestionArgs struct {
	TaskID    string                    `json:"taskId" jsonschema_description:"The task id"`
	Questions []AskUserQuestionQuestion `json:"questions" jsonschema_description:"1-4 questions to ask the human"`
	TimeoutMs int                       `json:"timeoutMs,omitempty" jsonschema_description:"How long to block waiting for an answer before returning {\"status\":\"pending\"} (default 60000). Call the tool again with the same questions to keep waiting — the human's answer, once submitted, is returned on whichever call is still blocked or the next one."`
}

// askUserQuestionHandler posts a 'question' transcript entry (answered via
// the web dashboard, not Discord — its plain-text threads can't render
// structured multiple-choice UI, see docs/adr/0018) and long-polls for the
// matching 'answer' entry, same shape as waitForMessagesHandler below. Only
// one question is ever outstanding per task at a time (the calling
// query() session blocks on this tool call before doing anything else), so
// no correlation ID is needed beyond "the next answer after this question's
// own seq" — the same assumption isApproval/isAbort already make elsewhere.
func askUserQuestionHandler(store transcript.Store) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewTypedToolHandler(func(ctx context.Context, _ mcp.CallToolRequest, args AskUserQuestionArgs) (*mcp.CallToolResult, error) {
		if args.TaskID == "" || len(args.Questions) == 0 {
			return mcp.NewToolResultError("taskId and at least one question are required"), nil
		}

		payload, err := json.Marshal(map[string]any{"questions": args.Questions})
		if err != nil {
			return nil, fmt.Errorf("ask_user_question: marshal questions: %w", err)
		}

		seq, err := store.Append(ctx, args.TaskID, "planner", string(payload), "question", uuid.NewString())
		if err != nil {
			return nil, fmt.Errorf("ask_user_question: %w", err)
		}

		timeoutMs := args.TimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = 60000
		}
		deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
		cursor := seq

		for {
			entries, nextSeq, err := store.ReadSince(ctx, args.TaskID, cursor, 1000)
			if err != nil {
				return nil, fmt.Errorf("ask_user_question: %w", err)
			}
			cursor = nextSeq
			for _, e := range entries {
				if e.From == "human" && e.Type == "answer" {
					return mcp.NewToolResultText(e.Text), nil
				}
			}
			if time.Now().After(deadline) {
				body, _ := json.Marshal(map[string]any{"status": "pending", "questionSeq": seq})
				return mcp.NewToolResultText(string(body)), nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(pollInterval):
			}
		}
	})
}
