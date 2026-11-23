package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/alertwebhook"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

// Alertmanager sole-router switch (infra-bootstrap ADR-0092): alerts now reach core
// through the bridge event (Dispatcher.alert) instead of the direct webhook. For an
// alert that came through Alertmanager, both paths must file under the SAME dedup
// key, or a delivery over both paths during the switch is filed twice. (A Grafana
// alert that fired under Grafana's own fingerprint via the old contact point is
// keyed differently; that one is filed again once, by design.)
//
// The fixtures in testdata/ are what the bridge stores as event Data: one alerts[]
// element of an Alertmanager v0.34 notification, verbatim (ukubi-bridge
// internal/alert Normalize). The Grafana one was captured from Grafana 13.2.4 ->
// Alertmanager v0.34.1 -> webhook on 2026-11-23. Pinning the bridge side (a golden
// test over Normalize that writes these files) lives in ukubi-bridge.

type keyRecorder struct{ keys []string }

func (k *keyRecorder) Create(_ context.Context, _, _, dedupKey, _, _, _ string) (string, bool, error) {
	k.keys = append(k.keys, dedupKey)
	return "p", true, nil
}
func (k *keyRecorder) DismissUnopened(context.Context, string, string) (bool, error) {
	return false, nil
}

func newAlertDispatcher(rec *keyRecorder) (*Dispatcher, *alertwebhook.Handler) {
	h := alertwebhook.New(rec, nil, alertwebhook.Config{Token: "t", Repo: "infra-bootstrap"})
	return &Dispatcher{alerts: h}, h
}

func TestBridgeAlertDedupKeyMatchesWebhook(t *testing.T) {
	for name, want := range map[string]string{
		"testdata/alert_prometheus_via_alertmanager.json": "abc123",
		"testdata/alert_grafana_via_alertmanager.json":    "fd821c9ea845b489",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			viaBridge, viaWebhook := &keyRecorder{}, &keyRecorder{}

			d, _ := newAlertDispatcher(viaBridge)
			ev := &bridgev1.CloudEvent{Id: want + ":firing:x", Source: "alertmanager",
				Type: typeAlertFiring, Subject: "alert/" + want, Actor: "machine", Data: raw}
			if err := d.alert(context.Background(), ev); err != nil {
				t.Fatal(err)
			}

			_, h := newAlertDispatcher(viaWebhook)
			req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager",
				strings.NewReader(`{"receiver":"bridge","status":"firing","alerts":[`+string(raw)+`]}`))
			req.Header.Set("Authorization", "Bearer t")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("webhook refused the alert: %d", w.Code)
			}

			if len(viaBridge.keys) != 1 || len(viaWebhook.keys) != 1 || viaBridge.keys[0] != viaWebhook.keys[0] || viaBridge.keys[0] != want {
				t.Fatalf("dedup keys differ: bridge %v, webhook %v, want [%s] both", viaBridge.keys, viaWebhook.keys, want)
			}
		})
	}
}
