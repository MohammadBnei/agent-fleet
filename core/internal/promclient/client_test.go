package promclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGet_TypedErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
		check  func(error) bool
	}{
		"query error": {`{"status":"error","error":"parse error"}`, 400, func(e error) bool {
			var qe *QueryError
			return errors.As(e, &qe) && qe.Msg == "parse error"
		}},
		"too large": {strings.Repeat("x", MaxBody+1), 200, func(e error) bool { return errors.Is(e, ErrTooLarge) }},
		"non-json 5xx": {"bad gateway", 502, func(e error) bool {
			var qe *QueryError
			return e != nil && !errors.As(e, &qe) && !errors.Is(e, ErrTooLarge)
		}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL).Query(context.Background(), "up")
		srv.Close()
		if !tc.check(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
