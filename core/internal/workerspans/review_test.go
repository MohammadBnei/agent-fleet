package workerspans

import (
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func TestSubmit_ALongTurnKeepsItsRoot(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	s := NewWithExporter(exp, DefaultLimits)
	var in []*agentfleetv1.WorkerSpan
	for i := 0; i < 600; i++ {
		in = append(in, sp("c"+strconv.Itoa(i), "root", "t"))
	}
	in = append(in, sp("root", "", "t")) // the root arrives last
	n := s.Submit("s", in)
	if n != DefaultLimits.Burst {
		t.Fatalf("accepted %d, want the burst (%d)", n, DefaultLimits.Burst)
	}
	var root bool
	for _, st := range drain(s, exp) {
		root = root || st.Name == "n-root"
	}
	if !root {
		t.Fatal("the turn's root span was dropped")
	}
}

func TestClip_CutsOnARuneBoundary(t *testing.T) {
	v := "aé€😀" // 1+2+3+4 bytes
	for n := 0; n <= len(v); n++ {
		if got := clip(v, n); !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("clip(%d) = %q", n, got)
		}
	}
	if got := clip("ééé", 3); got != "é" {
		t.Fatalf("clip = %q", got)
	}
}

func TestSubmit_AfterCloseIsRefusedNotAPanic(t *testing.T) {
	s := NewWithExporter(tracetest.NewInMemoryExporter(), DefaultLimits)
	s.Close()
	if n := s.Submit("s", []*agentfleetv1.WorkerSpan{sp("1", "", "t")}); n != 0 {
		t.Fatalf("accepted %d after Close", n)
	}
	s.Close() // twice is fine
}

func TestSubmit_CountCapIsARollingWindow(t *testing.T) {
	lim := Limits{MaxSpans: 2, MaxBytes: 1 << 20, Window: time.Hour, Rate: 1000, Burst: 1000, Queue: 8}
	s := NewWithExporter(tracetest.NewInMemoryExporter(), lim)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	two := func() []*agentfleetv1.WorkerSpan {
		return []*agentfleetv1.WorkerSpan{sp("1", "", "t"), sp("2", "", "t")}
	}
	if n := s.Submit("s", two()); n != 2 {
		t.Fatalf("first window: %d", n)
	}
	if n := s.Submit("s", two()); n != 0 {
		t.Fatalf("over the cap: %d", n)
	}
	now = now.Add(time.Hour)
	if n := s.Submit("s", two()); n != 2 {
		t.Fatalf("next window: %d, want 2", n)
	}
	s.Close()
}
