package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireDashboardHeader(t *testing.T) {
	called := false
	next := requireDashboardHeader(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	t.Run("missing header is rejected", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/t1/approve", nil)
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if called {
			t.Error("handler ran without the required header")
		}
	})

	t.Run("present header passes through", func(t *testing.T) {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/t1/approve", nil)
		req.Header.Set(dashboardHeader, "1")
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !called {
			t.Error("handler did not run despite the required header")
		}
	})
}
