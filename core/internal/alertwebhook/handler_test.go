package alertwebhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeTasks struct {
	calls    []string // external keys seen
	existing map[string]bool
}

func (f *fakeTasks) CreateDeduped(_ context.Context, kind, externalKey, _, _ string, _, _ *string) (string, bool, error) {
	f.calls = append(f.calls, kind+":"+externalKey)
	if f.existing[externalKey] {
		return "", false, nil
	}
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	f.existing[externalKey] = true
	return "task-" + externalKey, true, nil
}

type fakeDiscord struct{ threads int }

func (f *fakeDiscord) OpenThread(_, _, _ string) (string, error) {
	f.threads++
	return "thread-1", nil
}

func post(t *testing.T, h *Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const firing = `{"alerts":[{"status":"firing","fingerprint":"abc123","labels":{"alertname":"EtcdDown","namespace":"kube-system"},"annotations":{"summary":"etcd is down"}}]}`

// This endpoint creates tasks, and a thot task runs an agent with cluster
// access. An unauthenticated caller inside the cluster must not be able to
// spawn that.
func TestRejectsWrongToken(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Token: "right", Repo: "infra-bootstrap"})

	if rec := post(t, h, "wrong", firing); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", rec.Code)
	}
	if rec := post(t, h, "", firing); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rec.Code)
	}
	if len(tasks.calls) != 0 {
		t.Errorf("no task may be created without a valid token, got %v", tasks.calls)
	}
}

// A missing token must disable the endpoint, never serve it open — the
// failure mode of a misconfiguration should be "alerts don't arrive", not
// "anyone can spawn privileged work".
func TestUnconfiguredTokenDisablesEndpoint(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Repo: "infra-bootstrap"})

	rec := post(t, h, "anything", firing)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 when no token is configured", rec.Code)
	}
	if len(tasks.calls) != 0 {
		t.Errorf("no task may be created while disabled, got %v", tasks.calls)
	}
}

func TestCreatesThotTaskForFiringAlert(t *testing.T) {
	tasks := &fakeTasks{}
	dc := &fakeDiscord{}
	h := New(tasks, dc, Config{Token: "t", Repo: "infra-bootstrap"})

	if rec := post(t, h, "t", firing); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if len(tasks.calls) != 1 || tasks.calls[0] != "thot:abc123" {
		t.Errorf("expected one thot task keyed by fingerprint, got %v", tasks.calls)
	}
	if dc.threads != 1 {
		t.Errorf("expected a Discord thread, got %d", dc.threads)
	}
}

// Alertmanager re-sends a firing alert every repeat_interval. Without
// dedup, one flapping alert becomes an unbounded stream of pods and
// starves MAX_IN_FLIGHT_TASKS — a worse outage than the alert.
func TestRepeatedAlertIsDeduped(t *testing.T) {
	tasks := &fakeTasks{}
	dc := &fakeDiscord{}
	h := New(tasks, dc, Config{Token: "t", Repo: "infra-bootstrap"})

	post(t, h, "t", firing)
	rec := post(t, h, "t", firing)

	if !strings.Contains(rec.Body.String(), "deduped=1") {
		t.Errorf("second delivery should dedup, got %q", rec.Body.String())
	}
	if dc.threads != 1 {
		t.Errorf("a deduped alert must not open a second thread, got %d", dc.threads)
	}
}

// A resolved notification needs no investigation, and acting on one would
// double every alert's task count.
func TestIgnoresResolved(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Token: "t", Repo: "infra-bootstrap"})

	post(t, h, "t", `{"alerts":[{"status":"resolved","fingerprint":"abc123","labels":{"alertname":"EtcdDown"}}]}`)
	if len(tasks.calls) != 0 {
		t.Errorf("resolved alerts must not create tasks, got %v", tasks.calls)
	}
}

// No fingerprint means no dedup key, so accepting it reopens the storm
// risk the dedup exists to prevent.
func TestSkipsAlertWithoutFingerprint(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Token: "t", Repo: "infra-bootstrap"})

	post(t, h, "t", `{"alerts":[{"status":"firing","labels":{"alertname":"NoFingerprint"}}]}`)
	if len(tasks.calls) != 0 {
		t.Errorf("an unkeyed alert must be skipped, got %v", tasks.calls)
	}
}

// Losing Discord must not lose the task — it is already created and
// dispatching by then.
func TestDiscordFailureDoesNotFailTheRequest(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, failingDiscord{}, Config{Token: "t", Repo: "infra-bootstrap"})

	if rec := post(t, h, "t", firing); rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200 despite Discord failing", rec.Code)
	}
	if len(tasks.calls) != 1 {
		t.Errorf("task should still be created, got %v", tasks.calls)
	}
}

type failingDiscord struct{}

func (failingDiscord) OpenThread(_, _, _ string) (string, error) {
	return "", http.ErrBodyNotAllowed
}
