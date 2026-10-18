package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Per-request authorization (docs/adr/0069). Login checks the group too, but
// once login widens beyond platform-admins a perfectly valid cookie will belong
// to someone who must not reach DashboardService. Signature-valid is not
// authorized. (Not revocation: the groups checked are the cookie's own.)

// Every DashboardService method, enumerated from the generated descriptor so a
// method added later is covered without anyone remembering to list it.
func TestConnectInterceptor_NonAdminIsDeniedOnEveryDashboardMethod(t *testing.T) {
	s := mustSigner(t, "key-one")
	o := &OIDC{signer: s}
	path, h := agentfleetv1connect.NewDashboardServiceHandler(
		agentfleetv1connect.UnimplementedDashboardServiceHandler{},
		connect.WithInterceptors(o.ConnectInterceptor()),
	)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cookies := map[string]string{
		"no groups":         s.Sign(Identity{Email: "x@bnei.dev"}),
		"other groups only": s.Sign(Identity{Email: "x@bnei.dev", Groups: []string{"opus-users", "platform-admins-readonly"}}),
	}
	admin := s.Sign(Identity{Email: "me@bnei.dev", Groups: []string{RequiredGroup}})

	methods := agentfleetv1.File_agentfleet_v1_dashboard_proto.Services().ByName("DashboardService").Methods()
	if methods.Len() == 0 {
		t.Fatal("descriptor enumerated no methods; the table below would vacuously pass")
	}
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		procedure := "/" + agentfleetv1connect.DashboardServiceName + "/" + string(m.Name())
		if m.IsStreamingClient() {
			t.Fatalf("%s is client-streaming; extend call() before adding one", procedure)
		}
		call := func(cookie string) error {
			c := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := connect.NewRequest(&emptypb.Empty{})
			if cookie != "" {
				req.Header().Set("Cookie", SessionCookieName+"="+cookie)
			}
			if !m.IsStreamingServer() {
				_, err := c.CallUnary(ctx, req)
				return err
			}
			stream, err := c.CallServerStream(ctx, req)
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			for stream.Receive() {
			}
			return stream.Err()
		}

		for name, cookie := range cookies {
			if got := connect.CodeOf(call(cookie)); got != connect.CodePermissionDenied {
				t.Errorf("%s with %s: got %v, want permission_denied", procedure, name, got)
			}
		}
		if got := connect.CodeOf(call("")); got != connect.CodeUnauthenticated {
			t.Errorf("%s with no cookie: got %v, want unauthenticated", procedure, got)
		}
		// Positive control: the admin reaches the (unimplemented) handler. Without
		// it, an interceptor that denied everyone would pass this test.
		if got := connect.CodeOf(call(admin)); got != connect.CodeUnimplemented {
			t.Errorf("%s with admin cookie: got %v, want unimplemented (i.e. past auth)", procedure, got)
		}
	}
}

// A denial the interceptor makes must be visible: it carries the groups
// received, so a renamed group reads as that rather than as silence.
func TestConnectInterceptor_DenialIsLogged(t *testing.T) {
	logs := captureLog(t)
	s := mustSigner(t, "key-one")
	h := http.Header{}
	h.Set("Cookie", SessionCookieName+"="+s.Sign(Identity{Email: "x@bnei.dev", Groups: []string{"opus-users"}}))
	if _, err := (connectAuth{signer: s}).authorize(h, "/agentfleet.v1.DashboardService/ListSessions"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("got %v, want permission_denied", err)
	}
	for _, want := range []string{"refused request", "x@bnei.dev", "opus-users", "DashboardService/ListSessions", "platform-admins"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("denial log lacks %q: %s", want, logs)
		}
	}
}

// captureLog points slog's default logger at a buffer for one test. Not safe
// alongside t.Parallel, which this package does not use.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestGate_RefusesAValidCookieWithoutTheGroup(t *testing.T) {
	logs := captureLog(t)
	s := mustSigner(t, "key-one")
	o := &OIDC{signer: s}
	handler := o.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("non-admin reached %s", r.URL.Path)
	}))
	cookie := s.Sign(Identity{Email: "x@bnei.dev", Groups: []string{"opus-users"}})

	for _, p := range []string{"/", "/sessions/abc", "/some/new/route", "/agentfleet.v1.DashboardService/ListSessions"} {
		for _, mode := range []string{"navigate", "cors"} {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("Sec-Fetch-Mode", mode)
			req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			// 403, never a login redirect: login would refuse them again and the
			// browser would loop.
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s (%s): got %d, want 403", p, mode, rec.Code)
			}
		}
	}
	if !strings.Contains(logs.String(), "refused request") || !strings.Contains(logs.String(), "path=/sessions/abc") {
		t.Errorf("gate 403 left no log line: %s", logs)
	}

	// Exempt paths stay reachable for the same caller — /auth/me in particular
	// has to answer them, or the SPA cannot say who is signed in.
	open := o.Gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, p := range []string{"/healthz", "/webhook/alertmanager", "/auth/me", "/auth/callback"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: cookie})
		rec := httptest.NewRecorder()
		open.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("exempt %s: got %d, want 200", p, rec.Code)
		}
	}
}
