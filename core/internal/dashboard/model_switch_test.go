package dashboard

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/ratings"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// fakeSwitch records every call in order, so the teardown sequence is an assertion.
type fakeSwitch struct {
	t        *sessions.Session
	calls    []string
	listed   int
	goneAt   int // ListWorkerPods call after which the Job is gone; 0 = never
	last     ratings.Result
	haveLast bool
	appended string
}

func (f *fakeSwitch) Get(context.Context, string) (*sessions.Session, error) {
	f.calls = append(f.calls, "get")
	return f.t, nil
}
func (f *fakeSwitch) ClearLease(context.Context, string) error {
	f.calls = append(f.calls, "clear-lease")
	return nil
}
func (f *fakeSwitch) SetPodPhase(_ context.Context, _, phase, _ string) error {
	f.calls = append(f.calls, "phase:"+phase)
	return nil
}
func (f *fakeSwitch) SetModel(_ context.Context, _, m string) (bool, error) {
	f.calls = append(f.calls, "set-model:"+m)
	return true, nil
}
func (f *fakeSwitch) TearDownWorker(context.Context, string) error {
	f.calls = append(f.calls, "teardown")
	return nil
}
func (f *fakeSwitch) ListWorkerPods(context.Context) (map[string]string, error) {
	f.calls = append(f.calls, "list")
	f.listed++
	if f.goneAt > 0 && f.listed >= f.goneAt {
		return map[string]string{}, nil
	}
	return map[string]string{"s1": "Running"}, nil
}
func (f *fakeSwitch) LastResult(context.Context, string) (ratings.Result, bool, error) {
	return f.last, f.haveLast, nil
}

func (f *fakeSwitch) Append(_ context.Context, _, from, text, msgType, _ string) (int64, error) {
	f.calls = append(f.calls, "append:"+from+":"+msgType)
	f.appended = text
	return 1, nil
}

func (f *fakeSwitch) switcher() *modelSwitcher {
	return &modelSwitcher{
		lock: func(context.Context, string) (func(), error) {
			f.calls = append(f.calls, "lock")
			return func() { f.calls = append(f.calls, "unlock") }, nil
		},
		sessions: f, pods: f, transcr: f, results: f,
		poll: time.Millisecond, waitMax: 50 * time.Millisecond,
	}
}

func str(s string) *string { return &s }

func idleLive(model string) *sessions.Session {
	return &sessions.Session{ID: "s1", Model: str(model), PodPhase: str("POD_PHASE_RUNNING"), ActivitySeen: true, LastEntryType: str("result")}
}

func TestSwitchTeardownSequenceAndSeam(t *testing.T) {
	f := &fakeSwitch{t: idleLive("claude-opus-5"), goneAt: 3}
	w := f.switcher()
	w.gateway = true
	if err := w.switchModel(context.Background(), "s1", "kimi-k2-7-code"); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock", "get", "teardown", "list", "list", "list", "clear-lease", "phase:POD_PHASE_TERMINATED", "set-model:kimi-k2-7-code", "append:fleet:model_switch", "unlock"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v\nwant    %v", f.calls, want)
	}
	if f.appended != `{"from":"claude-opus-5","to":"kimi-k2-7-code"}` {
		t.Errorf("seam = %s", f.appended)
	}
}

func TestSwitchWithoutAPodTearsNothingDown(t *testing.T) {
	f := &fakeSwitch{t: &sessions.Session{ID: "s1", Model: str("claude-opus-5")}}
	if err := f.switcher().switchModel(context.Background(), "s1", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.calls, "teardown") || !slices.Contains(f.calls, "set-model:claude-sonnet-5") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestSwitchRefusals(t *testing.T) {
	working := idleLive("claude-opus-5")
	working.LastEntryType, working.LastEntryFrom = str("discussion"), str("human")
	now := time.Now()
	working.LastActiveAt = &now
	def := &sessions.Session{ID: "s1", DefinitionName: str("alert-responder")}
	swept := &sessions.Session{ID: "s1", Model: str("claude-opus-5"), SweptAt: &now}
	big := idleLive("claude-opus-5")

	tests := []struct {
		name  string
		t     *sessions.Session
		env   string
		model string
		last  *ratings.Result
		want  connect.Code
		msg   string
	}{
		{"definition session", def, "on", "claude-sonnet-5", nil, connect.CodeUnimplemented, "model switch is for repo sessions"},
		{"unknown model", idleLive("claude-opus-5"), "on", "gpt-9", nil, connect.CodeInvalidArgument, "invalid model"},
		{"gateway off", idleLive("claude-opus-5"), "", "zai-org-glm-5", nil, connect.CodeFailedPrecondition, "gateway models are disabled (FLEET_GATEWAY_MODELS)"},
		{"turn running", working, "on", "claude-sonnet-5", nil, connect.CodeFailedPrecondition, "a turn is running"},
		{"swept", swept, "on", "claude-sonnet-5", nil, connect.CodeFailedPrecondition, "swept"},
		{"window too small", big, "on", "zai-org-glm-5", &ratings.Result{ContextTokens: 210000}, connect.CodeFailedPrecondition, "takes 198000"},
		{"claude window too small", big, "on", "claude-sonnet-5", &ratings.Result{ContextTokens: 250000}, connect.CodeFailedPrecondition, "takes 200000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSwitch{t: tc.t, goneAt: 1}
			if tc.last != nil {
				f.last, f.haveLast = *tc.last, true
			}
			w := f.switcher()
			w.gateway = tc.env == "on"
			err := w.switchModel(context.Background(), "s1", tc.model)
			if connect.CodeOf(err) != tc.want || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v (code %v), want %v containing %q", err, connect.CodeOf(err), tc.want, tc.msg)
			}
			for _, c := range f.calls {
				if c == "teardown" || strings.HasPrefix(c, "set-model") || strings.HasPrefix(c, "append") {
					t.Fatalf("refused switch still ran %s: %v", c, f.calls)
				}
			}
		})
	}
}

func TestSwitchToClaudeThatFitsIsAllowed(t *testing.T) {
	f := &fakeSwitch{t: idleLive("zai-org-glm-5"), goneAt: 1, last: ratings.Result{ContextTokens: 150000}, haveLast: true}
	if err := f.switcher().switchModel(context.Background(), "s1", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchLeavesModelUnsavedWhenTheJobStays(t *testing.T) {
	f := &fakeSwitch{t: idleLive("claude-opus-5")} // goneAt 0: never goes
	err := f.switcher().switchModel(context.Background(), "s1", "claude-sonnet-5")
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.calls {
		if c == "clear-lease" || strings.HasPrefix(c, "set-model") || strings.HasPrefix(c, "phase:") {
			t.Fatalf("ran %s after a failed wait: %v", c, f.calls)
		}
	}
}

func TestSwitchToTheSameModelIsANoOp(t *testing.T) {
	f := &fakeSwitch{t: idleLive("claude-opus-5")}
	if err := f.switcher().switchModel(context.Background(), "s1", "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.calls, "teardown") {
		t.Fatal("tore down for a no-op")
	}
}
