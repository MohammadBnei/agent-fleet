package mcpserver

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func allTools() []mcp.Tool {
	var out []mcp.Tool
	for _, st := range newMCPServer(nil, nil, nil).ListTools() {
		out = append(out, st.Tool)
	}
	return out
}

func names(ts []mcp.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

func TestGrantFilter(t *testing.T) {
	all := allTools()
	ctx := context.Background()
	granted := []string{"AskUserQuestion", "view_logs", "query_metrics", "not_a_tool"}
	def := grantFilter(func(context.Context) ([]string, bool, error) { return granted, true, nil })
	want := []string{"AskUserQuestion", "query_metrics", "view_logs"}
	if got := names(def(ctx, all)); !slices.Equal(got, want) {
		t.Fatalf("definition pod advertises %v, want %v", got, want)
	}
	repo := grantFilter(func(context.Context) ([]string, bool, error) { return nil, false, nil })
	if got := repo(ctx, all); len(got) != len(all) {
		t.Fatalf("repo pod advertises %d of %d tools", len(got), len(all))
	}
}

func TestGrantFilterRetriesAfterError(t *testing.T) {
	all := allTools()
	calls := 0
	f := grantFilter(func(context.Context) ([]string, bool, error) {
		calls++
		if calls == 1 {
			return nil, false, errors.New("core down")
		}
		return []string{"view_logs"}, true, nil
	})
	if got := f(context.Background(), all); len(got) != len(all) {
		t.Fatalf("failed read must advertise all, got %d", len(got))
	}
	if got := names(f(context.Background(), all)); !slices.Equal(got, []string{"view_logs"}) {
		t.Fatalf("second listing = %v", got)
	}
	f(context.Background(), all)
	if calls != 2 {
		t.Fatalf("spec should be cached after a good read, calls=%d", calls)
	}
}

// mcp-go runs the filter on tools/call too, with the one called tool.
func TestGrantFilterCallTimeShape(t *testing.T) {
	ctx := context.Background()
	def := grantFilter(func(context.Context) ([]string, bool, error) {
		return []string{"view_logs"}, true, nil
	})
	if got := def(ctx, []mcp.Tool{{Name: "set_session_meta"}}); len(got) != 0 {
		t.Fatalf("ungranted call passed the filter: %v", names(got))
	}
	if got := names(def(ctx, []mcp.Tool{{Name: "view_logs"}})); !slices.Equal(got, []string{"view_logs"}) {
		t.Fatalf("granted call filtered out: %v", got)
	}
}
