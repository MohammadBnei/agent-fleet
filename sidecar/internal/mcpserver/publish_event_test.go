package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakePublisher struct {
	got []string
	err error
}

func (f *fakePublisher) PublishEvent(_ context.Context, typ, subject, text, key string) (*agentfleetv1.PublishEventResponse, error) {
	f.got = append(f.got, typ+"|"+subject+"|"+text+"|"+key)
	if f.err != nil {
		return nil, f.err
	}
	return &agentfleetv1.PublishEventResponse{Id: "e1", Trusted: true, Hops: 2}, nil
}

func TestPublishEvent(t *testing.T) {
	var listed bool
	for _, tl := range newMCPServer(nil).ListTools() {
		if tl.Tool.Name == "publish_event" {
			listed = true
			for _, k := range []string{"actor", "trusted", "hops", "causationId"} {
				if _, ok := tl.Tool.InputSchema.Properties[k]; ok {
					t.Fatalf("publish_event lets the agent set %s", k)
				}
			}
		}
	}
	if !listed {
		t.Fatal("publish_event is not registered")
	}
	call := func(f *fakePublisher, args map[string]any) *mcp.CallToolResult {
		req := mcp.CallToolRequest{}
		req.Params.Name = "publish_event"
		req.Params.Arguments = args
		res, err := publishEventHandler(f)(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	f := &fakePublisher{}
	if res := call(f, map[string]any{"type": "dev.bnei.agent.x"}); !res.IsError || len(f.got) != 0 {
		t.Fatal("a call without subject and text went to core")
	}
	res := call(f, map[string]any{"type": "dev.bnei.agent.x", "subject": "s", "text": "do it", "idempotencyKey": "k"})
	if res.IsError || len(f.got) != 1 || f.got[0] != "dev.bnei.agent.x|s|do it|k" {
		t.Fatalf("published %v: %+v", f.got, res)
	}
	if txt := res.Content[0].(mcp.TextContent).Text; !strings.Contains(txt, `"hops":2`) {
		t.Fatalf("result %s", txt)
	}
	f.err = errors.New("this definition does not grant publish_event")
	if res := call(f, map[string]any{"type": "dev.bnei.agent.x", "subject": "s", "text": "t"}); !res.IsError {
		t.Fatal("a refusal was not an error result")
	}
}
