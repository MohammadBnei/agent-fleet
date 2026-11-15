package localapi

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

// The worker's timings ride this pod's one CoreService connection to core,
// which owns identity, caps and export (docs/adr/0081 §7). Nothing is kept
// here: when core is down the batch is dropped, because the worker neither
// waits on nor needs an answer, and a buffer would be one more thing to bound.
const (
	spansMaxBody  = 1 << 20
	spansMaxBatch = 500
	// spansTimeout bounds the call: the connection waits for ready and
	// retries (coreclient), which is right for a message and wrong for a span.
	spansTimeout = 3 * time.Second
)

type spanJSON struct {
	Name   string         `json:"name"`
	ID     string         `json:"id"`
	Parent string         `json:"parent"`
	Turn   string         `json:"turn"`
	Start  int64          `json:"startNs"`
	End    int64          `json:"endNs"`
	Status string         `json:"status"` // "ok" | "error" | ""
	Attrs  map[string]any `json:"attrs"`
}

func spansHandler(core *coreclient.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Spans []spanJSON `json:"spans"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, spansMaxBody)
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if len(body.Spans) > spansMaxBatch {
			// Cut a long batch from the children, never the roots they hang from.
			sort.SliceStable(body.Spans, func(i, j int) bool {
				return body.Spans[i].Parent == "" && body.Spans[j].Parent != ""
			})
			body.Spans = body.Spans[:spansMaxBatch]
		}
		out := make([]*agentfleetv1.WorkerSpan, 0, len(body.Spans))
		for _, s := range body.Spans {
			out = append(out, toWorkerSpan(s))
		}
		ctx, cancel := context.WithTimeout(r.Context(), spansTimeout)
		defer cancel()
		n, err := core.ExportSpans(ctx, out)
		if err != nil {
			writeError(w, http.StatusBadGateway, err) // dropped; the worker moves on
			return
		}
		writeJSON(w, http.StatusOK, map[string]int32{"accepted": n})
	}
}

func toWorkerSpan(s spanJSON) *agentfleetv1.WorkerSpan {
	w := &agentfleetv1.WorkerSpan{
		Name: s.Name, LocalId: s.ID, ParentLocalId: s.Parent, TurnKey: s.Turn,
		StartUnixNano: s.Start, EndUnixNano: s.End,
	}
	switch s.Status {
	case "ok":
		w.Status = agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_OK
	case "error":
		w.Status = agentfleetv1.WorkerSpanStatus_WORKER_SPAN_STATUS_ERROR
	}
	for k, v := range s.Attrs {
		switch v := v.(type) {
		case string:
			if w.Attributes == nil {
				w.Attributes = map[string]string{}
			}
			w.Attributes[k] = v
		case float64: // JSON numbers; the worker sends integers (counts, micro-USD)
			if w.IntAttributes == nil {
				w.IntAttributes = map[string]int64{}
			}
			w.IntAttributes[k] = int64(math.Round(v))
		case bool:
			if w.Attributes == nil {
				w.Attributes = map[string]string{}
			}
			w.Attributes[k] = strconv.FormatBool(v)
		}
	}
	return w
}
