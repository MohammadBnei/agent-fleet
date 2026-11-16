package responder

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeAgents struct {
	invokes []*agentfleetv1.InvokeRequest
	fail    []error // one per Invoke call, nil = success
	results []*agentfleetv1.WaitForResultResponse
}

func (f *fakeAgents) AsClient(ctx context.Context, _ string) (context.Context, error) {
	return ctx, nil
}
func (f *fakeAgents) InvokeUntrusted(_ context.Context, r *connect.Request[agentfleetv1.InvokeRequest]) (*connect.Response[agentfleetv1.InvokeResponse], error) {
	f.invokes = append(f.invokes, r.Msg)
	if i := len(f.invokes) - 1; i < len(f.fail) && f.fail[i] != nil {
		return nil, f.fail[i]
	}
	return connect.NewResponse(&agentfleetv1.InvokeResponse{ThreadId: "th"}), nil
}
func (f *fakeAgents) WaitForResult(context.Context, *connect.Request[agentfleetv1.WaitForResultRequest]) (*connect.Response[agentfleetv1.WaitForResultResponse], error) {
	r := f.results[0]
	f.results = f.results[1:]
	return connect.NewResponse(r), nil
}

func newRunner(f *fakeAgents, fi *fakeFiler) *Runner {
	return &Runner{
		Ctx: context.Background(), Client: "bridge", Agents: f, Filer: fi.filer(),
		Defs: reg{[]definitions.Head{{Name: "resp", SHA: "s", Effective: spec}}},
		Wait: func(run func()) { run() },
	}
}

func TestRunnerInvokesUntrustedThenFiles(t *testing.T) {
	f := &fakeAgents{results: []*agentfleetv1.WaitForResultResponse{
		{State: "running"}, {State: "waiting"},
		{State: "done", StructuredOutput: `{"outcome":"catalogue","selection":{"action":"restart","name":"api"}}`},
	}}
	fi := &fakeFiler{}
	newRunner(f, fi).Start("resp", Group{Key: "0123456789abcdef"}, []json.RawMessage{json.RawMessage(`{}`)})
	if len(f.invokes) != 1 || f.invokes[0].GetDefinition() != "resp" || !strings.Contains(f.invokes[0].GetInput(), "<untrusted-alert>") ||
		!strings.HasPrefix(f.invokes[0].GetIdempotencyKey(), "alert:0123456789abcdef:") {
		t.Fatalf("invoke: %+v", f.invokes)
	}
	if len(fi.selected) != 1 {
		t.Fatalf("the run's selection is filed once it is done, not before: %v", fi.selected)
	}
}

func TestRunnerRetriesAFullFleetWithTheSameKey(t *testing.T) {
	old := InvokeRetry
	InvokeRetry = time.Millisecond
	t.Cleanup(func() { InvokeRetry = old })
	f := &fakeAgents{
		fail:    []error{connect.NewError(connect.CodeResourceExhausted, errors.New("full")), nil},
		results: []*agentfleetv1.WaitForResultResponse{{State: "done", StructuredOutput: `{"outcome":"nothing"}`}},
	}
	newRunner(f, &fakeFiler{}).Start("resp", Group{Key: "k"}, nil)
	if len(f.invokes) != 2 || f.invokes[0].GetIdempotencyKey() != f.invokes[1].GetIdempotencyKey() {
		t.Fatalf("a retry is the same run: %+v", f.invokes)
	}
}

func TestRunnerFilesACardForARunWithNoResult(t *testing.T) {
	for name, res := range map[string]*agentfleetv1.WaitForResultResponse{
		"failed":    {State: "failed"},
		"cancelled": {State: "cancelled"},
		"bad":       {State: "done", StructuredOutput: `{"outcome":"bogus"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAgents{results: []*agentfleetv1.WaitForResultResponse{res}}
			fi := &fakeFiler{}
			newRunner(f, fi).Start("resp", Group{Key: "k", Name: "A"}, []json.RawMessage{json.RawMessage(`{}`)})
			if len(fi.selected) != 0 || len(fi.drafts) != 1 || !strings.Contains(fi.drafts[0].Ask, "could not answer") {
				t.Fatalf("one card for a run with no result: %+v", fi.drafts)
			}
		})
	}
}

func TestRunnerFallsBackToAProposalWhenNoRunStarts(t *testing.T) {
	f := &fakeAgents{fail: []error{connect.NewError(connect.CodeFailedPrecondition, errors.New("no letter"))}}
	r := newRunner(f, &fakeFiler{})
	var filed []string
	r.Fallback = func(_ context.Context, raw json.RawMessage) { filed = append(filed, string(raw)) }
	r.Start("resp", Group{Key: "k"}, []json.RawMessage{json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)})
	if len(f.invokes) != 1 || len(filed) != 2 {
		t.Fatalf("a refused invoke files each alert as a proposal, not nothing: invokes %d filed %v", len(f.invokes), filed)
	}
}

func TestRunnerKeysPerDefinition(t *testing.T) {
	f := &fakeAgents{results: []*agentfleetv1.WaitForResultResponse{{State: "done", StructuredOutput: `{"outcome":"nothing"}`}, {State: "done", StructuredOutput: `{"outcome":"nothing"}`}}}
	r := newRunner(f, &fakeFiler{})
	r.Start("one", Group{Key: "k"}, nil)
	r.Start("two", Group{Key: "k"}, nil)
	if len(f.invokes) != 2 || f.invokes[0].GetIdempotencyKey() == f.invokes[1].GetIdempotencyKey() {
		t.Fatalf("two responders on one group need two keys: %+v", f.invokes)
	}
}

func TestRunnerStopsOnARefusal(t *testing.T) {
	f := &fakeAgents{fail: []error{connect.NewError(connect.CodeFailedPrecondition, errors.New("no letter"))}}
	newRunner(f, &fakeFiler{}).Start("resp", Group{Key: "k"}, nil)
	if len(f.invokes) != 1 {
		t.Fatalf("a refusal is not retried: %d", len(f.invokes))
	}
}
