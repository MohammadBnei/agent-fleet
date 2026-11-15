package localapi

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// A batch over the cap is cut from its children: the root, even when it comes
// last, still reaches core.
func TestSpans_LongBatchKeepsTheRoot(t *testing.T) {
	srv, fake, _ := spansEnv(t)
	var sb strings.Builder
	sb.WriteString(`{"spans":[`)
	for i := 0; i < 600; i++ {
		sb.WriteString(`{"name":"c","id":"c` + strconv.Itoa(i) + `","parent":"root","turn":"u","startNs":1,"endNs":2},`)
	}
	sb.WriteString(`{"name":"invoke_agent","id":"root","turn":"u","startNs":1,"endNs":2}]}`)
	resp, err := http.Post(srv.URL+"/spans", "application/json", strings.NewReader(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	fake.mu.Lock()
	spans := fake.req.GetSpans()
	fake.mu.Unlock()
	if len(spans) != spansMaxBatch {
		t.Fatalf("forwarded %d, want %d", len(spans), spansMaxBatch)
	}
	if spans[0].GetName() != "invoke_agent" {
		t.Fatalf("root was cut: first span is %q", spans[0].GetName())
	}
}
