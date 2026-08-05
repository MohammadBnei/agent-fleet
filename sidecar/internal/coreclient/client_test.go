package coreclient

import "testing"

// grpc.NewClient only validates retryServiceConfig's JSON at call time —
// go build/vet/lint never catch a bad status-code string in it (shipped
// once: "CANCELED" instead of grpc's expected "CANCELLED", which crash-
// looped every pod calling New at startup). This is the cheapest possible
// guard against that recurring silently.
func TestNew_ValidServiceConfig(t *testing.T) {
	c, err := New("localhost:0", "task-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Close()
}
