package mcpserver

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// These exercise the handlers' own arg-validation guard clauses — passing
// a nil *coreclient.Client proves core is never touched when the required
// arg is missing, no bufconn/fake gRPC server needed. The actual proxy-to-
// core round trip is a one-line passthrough, covered by coreclient's own
// tests and the manual verification steps (docs/adr/0030).
func TestGetSharedFileUploadURLHandler_RequiresFilename(t *testing.T) {
	h := getSharedFileUploadURLHandler(nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"contentType": "text/plain"}

	result, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for missing filename")
	}
}

func TestGetSharedFileDownloadURLHandler_RequiresKey(t *testing.T) {
	h := getSharedFileDownloadURLHandler(nil)
	req := mcp.CallToolRequest{}

	result, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for missing key")
	}
}

func TestDeleteSharedFileHandler_RequiresKey(t *testing.T) {
	h := deleteSharedFileHandler(nil)
	req := mcp.CallToolRequest{}

	result, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for missing key")
	}
}
