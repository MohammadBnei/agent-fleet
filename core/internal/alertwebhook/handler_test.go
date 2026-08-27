package alertwebhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeTasks struct {
	calls    []string // dedup keys seen
	payloads []string // raw alert JSON handed to Create, in call order
	bodies   []string
	existing map[string]bool
}

func (f *fakeTasks) Create(_ context.Context, _, source, dedupKey, _, body, payloadJSON string) (string, bool, error) {
	f.calls = append(f.calls, source+":"+dedupKey)
	f.payloads = append(f.payloads, payloadJSON)
	f.bodies = append(f.bodies, body)
	if f.existing[dedupKey] {
		return "", false, nil
	}
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	f.existing[dedupKey] = true
	return "proposal-" + dedupKey, true, nil
}

type fakeDiscord struct{ threads int }

func (f *fakeDiscord) Notify(_ context.Context, _ string) error {
	f.threads++
	return nil
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

// An alert carrying detail that describe()'s allowlist drops: a `container`
// label, a non-summary annotation, and generatorURL.
const firingRich = `{"alerts":[{"status":"firing","fingerprint":"rich1","startsAt":"2026-08-26T10:00:00Z","generatorURL":"https://prom.bnei.lan/graph?g0.expr=up==0","labels":{"alertname":"PodCrashLooping","container":"api","namespace":"prod"},"annotations":{"runbook_url":"https://runbook/pcl"}}]}`

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
	if len(tasks.calls) != 1 || tasks.calls[0] != "alert:abc123" {
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

func (failingDiscord) Notify(_ context.Context, _ string) error {
	return http.ErrBodyNotAllowed
}

// The whole point of keeping the raw payload: describe() flattens an alert
// through a hardcoded allowlist, so a rule whose annotations it does not know
// about produced a proposal that showed the boilerplate and nothing else —
// and the detail was unrecoverable, since core holds no Alertmanager client
// to ask again.
func TestStoresRawAlertPayload(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Token: "t", Repo: "infra-bootstrap"})

	if rec := post(t, h, "t", firingRich); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if len(tasks.payloads) != 1 {
		t.Fatalf("expected one proposal, got %d", len(tasks.payloads))
	}

	var got struct {
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     string            `json:"startsAt"`
		GeneratorURL string            `json:"generatorURL"`
	}
	if err := json.Unmarshal([]byte(tasks.payloads[0]), &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%q)", err, tasks.payloads[0])
	}
	// Each of these is dropped by describe(), which is why the flattened body
	// cannot stand in for the payload.
	if got.Labels["container"] != "api" {
		t.Errorf("container label lost: %v", got.Labels)
	}
	if got.Annotations["runbook_url"] != "https://runbook/pcl" {
		t.Errorf("non-summary annotation lost: %v", got.Annotations)
	}
	if got.GeneratorURL == "" || got.StartsAt == "" {
		t.Errorf("generatorURL/startsAt lost: %+v", got)
	}
	if strings.Contains(tasks.bodies[0], "container") {
		t.Errorf("body is still the flattened instruction, not the payload: %q", tasks.bodies[0])
	}
}

// A batch is one HTTP request; one unparseable member must not take the good
// alerts with it, because Alertmanager retries the batch as a unit.
func TestMalformedAlertDoesNotDropTheBatch(t *testing.T) {
	tasks := &fakeTasks{}
	h := New(tasks, nil, Config{Token: "t", Repo: "infra-bootstrap"})

	body := `{"alerts":[["not an object"],{"status":"firing","fingerprint":"ok1","labels":{"alertname":"Fine"}}]}`
	if rec := post(t, h, "t", body); rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if len(tasks.calls) != 1 || tasks.calls[0] != "alert:ok1" {
		t.Errorf("the well-formed alert should still be filed, got %v", tasks.calls)
	}
}
