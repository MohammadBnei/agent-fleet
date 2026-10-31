package errkeys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/MohammadBnei/ukubi-go/errs"
	"github.com/MohammadBnei/ukubi-go/errs/connectkeys"
	"github.com/MohammadBnei/ukubi-go/errs/connectkeys/wiring"
	errorsv1 "github.com/MohammadBnei/ukubi-go/proto/gen/go/ukubi/errors/v1"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// TestConstructors: the code and words are the caller's; the key rides as the detail, and Of reads it back.
func TestConstructors(t *testing.T) {
	ce := Connect(connect.CodeFailedPrecondition, KeyDefinitionChanged, errors.New("DEFINITION_CHANGED: start a new thread"))
	if ce.Code() != connect.CodeFailedPrecondition || ce.Message() != "DEFINITION_CHANGED: start a new thread" || Of(ce) != KeyDefinitionChanged {
		t.Errorf("connect: %v %q", ce, Of(ce))
	}
	// The wire is errkeys' own: one agentfleet.v1.ErrorKey detail, the bytes of {key}, which islands reads by hand.
	want, err := proto.Marshal(&agentfleetv1.ErrorKey{Key: string(KeyDefinitionChanged)})
	if err != nil {
		t.Fatal(err)
	}
	if d := ce.Details(); len(d) != 1 || d[0].Type() != "agentfleet.v1.ErrorKey" || !bytes.Equal(d[0].Bytes(), want) {
		t.Errorf("connect detail: %v", d)
	}
	ge := Status(codes.PermissionDenied, KeyInternal, "nope")
	if s, _ := status.FromError(ge); s.Code() != codes.PermissionDenied || s.Message() != "nope" || Of(ge) != KeyInternal {
		t.Errorf("grpc: %v %q", ge, Of(ge))
	}
	if Of(errors.New("plain")) != "" || Of(nil) != "" || Of(connect.NewError(connect.CodeInternal, errors.New("x"))) != "" {
		t.Error("an unkeyed error has a key") // the positive control for Of
	}
}

// TestWire: over a real Connect client and a real gRPC client, a handler's unkeyed error arrives keyed internal,
// with its code (an Unimplemented service: every method answers Unimplemented).
func TestWire(t *testing.T) {
	path, h := agentfleetv1connect.NewAgentServiceHandler(agentfleetv1connect.UnimplementedAgentServiceHandler{}, WithInterceptors())
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := agentfleetv1connect.NewAgentServiceClient(srv.Client(), srv.URL).ListThreads(context.Background(), connect.NewRequest(&agentfleetv1.ListThreadsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnimplemented || Of(err) != KeyInternal {
		t.Errorf("connect: %v key %q", err, Of(err))
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(UnaryServerInterceptor()), grpc.ChainStreamInterceptor(StreamServerInterceptor()))
	agentfleetv1.RegisterCoreServiceServer(gs, agentfleetv1.UnimplementedCoreServiceServer{})
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, err = agentfleetv1.NewCoreServiceClient(conn).GetSession(context.Background(), &agentfleetv1.GetSessionRequest{})
	if status.Code(err) != codes.Unimplemented || Of(err) != KeyInternal {
		t.Errorf("grpc: %v key %q", err, Of(err))
	}
}

// TestErrKeys: every key a call to Connect or Status names is one of errkeys.go's errs.Key constants, named directly;
// no key is made outside this package (ukubi-go's errs and connectkeys are imported here only: no errs.Key conversion,
// no second Keys), Connect and Status are only ever called (never taken as a value, so an alias cannot carry a key
// past this check), and errkeys is never dot-imported. Syntactic, over core's non-test files outside this package.
func TestErrKeys(t *testing.T) {
	keys := map[string]bool{}
	for name := range registry(t) {
		keys[name] = true
	}
	checked := 0
	walkCore(t, func(p string, file *ast.File) {
		name := ""
		for _, im := range file.Imports {
			switch v, _ := strconv.Unquote(im.Path.Value); v {
			case "github.com/MohammadBnei/agent-fleet/core/internal/errkeys":
				name = "errkeys"
				if im.Name != nil {
					name = im.Name.Name
				}
			case "github.com/MohammadBnei/ukubi-go/errs", "github.com/MohammadBnei/ukubi-go/errs/connectkeys":
				t.Errorf("%s: imports %s: keys are made in errkeys only", p, v)
			}
		}
		if name == "." {
			t.Errorf("%s: errkeys dot-imported", p)
			return
		}
		if name == "" {
			return
		}
		called := map[*ast.SelectorExpr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || (s.Sel.Name != "Connect" && s.Sel.Name != "Status") || fmtIdent(s.X) != name {
				return true
			}
			called[s] = true
			checked++
			k, ok := c.Args[1].(*ast.SelectorExpr)
			if !ok || fmtIdent(k.X) != name || !keys[k.Sel.Name] {
				t.Errorf("%s: errkeys.%s with a key that is not one of errkeys.go's constants", p, s.Sel.Name)
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			s, ok := n.(*ast.SelectorExpr)
			if !ok || fmtIdent(s.X) != name {
				return true
			}
			if (s.Sel.Name == "Connect" || s.Sel.Name == "Status") && !called[s] {
				t.Errorf("%s: errkeys.%s taken as a value: call it, with its key named", p, s.Sel.Name)
			}
			return true
		})
	})
	if len(keys) < 2 || checked < 1 {
		t.Errorf("%d keys, %d calls checked: the check no longer sees them", len(keys), checked) // the positive control
	}
}

// registry is errkeys.go's constants, name to wire string: each an errs.Key, no wire string twice.
func registry(t *testing.T) map[string]string {
	t.Helper()
	keys, names := map[string]string{}, map[string]string{}
	f, err := parser.ParseFile(token.NewFileSet(), "errkeys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				if st, ok := vs.Type.(*ast.SelectorExpr); !ok || fmtIdent(st.X) != "errs" || st.Sel.Name != "Key" {
					t.Errorf("errkeys.go: a constant %s that is not an errs.Key", vs.Names[0].Name)
					continue
				}
				for i, n := range vs.Names {
					v, err := strconv.Unquote(vs.Values[i].(*ast.BasicLit).Value)
					if err != nil {
						t.Fatal(err)
					}
					if other, dup := names[v]; dup {
						t.Errorf("errkeys.go: %s and %s are both %q", other, n.Name, v)
					}
					keys[n.Name], names[v] = v, n.Name
				}
			}
		}
	}
	return keys
}

// TestDeclared: the registry and the services' key enums, agent.proto's AgentErrorKey and core.proto's CoreErrorKey,
// are one set of keys, each RPC's (ukubi.errors.v1.keys) among them (connectkeys.CheckDeclared). Each value's wire
// string is its service's prefix, "agent." or "core.", and its name after the enum's prefix in lower case: the islands
// and the dashboard map the generated enum's names, which protoc-gen-es strips of the prefix.
func TestDeclared(t *testing.T) {
	var reg []errs.Key
	for _, v := range registry(t) {
		reg = append(reg, errs.Key(v))
	}
	if err := connectkeys.CheckDeclared(reg,
		agentfleetv1.File_agentfleet_v1_agent_proto.Services().ByName("AgentService"),
		agentfleetv1.File_agentfleet_v1_core_proto.Services().ByName("CoreService")); err != nil {
		t.Error(err)
	}
	for _, tc := range []struct {
		enum         protoreflect.EnumDescriptor
		prefix, wire string
	}{
		{agentfleetv1.AgentErrorKey(0).Descriptor(), "AGENT_ERROR_KEY_", "agent."},
		{agentfleetv1.CoreErrorKey(0).Descriptor(), "CORE_ERROR_KEY_", "core."},
	} {
		vals := tc.enum.Values()
		for i := 1; i < vals.Len(); i++ {
			v := vals.Get(i)
			want := tc.wire + strings.ToLower(strings.TrimPrefix(string(v.Name()), tc.prefix))
			if got := proto.GetExtension(v.Options(), errorsv1.E_Key).(string); got != want {
				t.Errorf("%s: wire string %q, want %q", v.Name(), got, want)
			}
		}
		if vals.Len() < 2 {
			t.Errorf("%s declares no key: the check no longer sees them", tc.enum.Name()) // the positive control
		}
	}
}

func fmtIdent(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// TestWiring: every Connect handler core builds takes this package's WithInterceptors first, and every gRPC server
// opens with its chains (connectkeys/wiring, over core's non-test files).
func TestWiring(t *testing.T) {
	findings, handlers, servers, err := wiring.Check(filepath.Join("..", ".."), "github.com/MohammadBnei/agent-fleet/core/internal/errkeys")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if handlers < 3 || servers < 1 {
		t.Errorf("%d Connect handlers, %d gRPC servers: the check no longer sees them", handlers, servers) // the positive control
	}
}

// refuse is an interceptor that answers every call with a plain error, the way a gate interceptor (CSRF, auth) does.
type refuse struct{}

func (refuse) WrapUnary(connect.UnaryFunc) connect.UnaryFunc {
	return func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, errors.New("refused")
	}
}
func (refuse) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (refuse) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error { return errors.New("refused") }
}

// TestOutermost: WithInterceptors puts the fallback outside the handler's own interceptors, so an error one of them
// returns, before the handler runs, leaves keyed too.
func TestOutermost(t *testing.T) {
	path, h := agentfleetv1connect.NewAgentServiceHandler(agentfleetv1connect.UnimplementedAgentServiceHandler{}, WithInterceptors(refuse{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := agentfleetv1connect.NewAgentServiceClient(srv.Client(), srv.URL).ListThreads(context.Background(), connect.NewRequest(&agentfleetv1.ListThreadsRequest{}))
	if Of(err) != KeyInternal || connect.CodeOf(err) != connect.CodeUnknown || !strings.Contains(err.Error(), "refused") {
		t.Errorf("an interceptor's refusal: %v key %q", err, Of(err))
	}
}

// TestInternalIsConnectkeys: the fallback's key and errkeys' are one wire string, server.internal.
func TestInternalIsConnectkeys(t *testing.T) {
	if KeyInternal != "server.internal" {
		t.Errorf("KeyInternal %q", KeyInternal)
	}
	if KeyInternal != connectkeys.KeyInternal {
		t.Errorf("connectkeys' %q", connectkeys.KeyInternal)
	}
}

// TestBareSentinel: a shared sentinel with no meta, as agentsvc's errNoThread is, returned by concurrent requests
// through the fallback. Meta() makes its map on its first call, a write, so a fallback that reads the sentinel's
// Meta() races (run with -race; errkeys' own copy did, docs/adr/0081). Every request gets its own keyed copy, and the
// sentinel stays bare.
func TestBareSentinel(t *testing.T) {
	sentinel := connect.NewError(connect.CodeNotFound, errors.New("no such thread"))
	h := connect.NewUnaryHandler("/test.v1.T/Get", func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
		return nil, sentinel
	}, WithInterceptors())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/test.v1.T/Get", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "agentfleet.v1.ErrorKey") {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	wg.Wait()
	if Of(sentinel) != "" || len(sentinel.Details()) != 0 {
		t.Errorf("the sentinel was changed: %d details", len(sentinel.Details()))
	}
}

// bareNewError is where core keeps connect.NewError, file to the declaration holding it: shared sentinels, built
// once at init (a location there names no request's return), and helpers that map an error for their callers (the
// location would name the helper, not the caller). Everywhere else errkeys.Connect builds it, so its access log line
// carries a source.
var bareNewError = map[string][]string{
	"internal/agentsvc/deck.go":    {"errNoDecision"},
	"internal/agentsvc/threads.go": {"errNoThread"},
	"internal/dashboard/server.go": {"AnswerErr", "errInactive"},
	"internal/dashboard/grants.go": {"grantErr"},
}

// TestNoBareNewError: core's non-test, non-generated files outside this package call connect.NewError only in
// bareNewError's declarations, and each of those still does (the positive control: a renamed one fails here, not
// silently). Syntactic, as TestErrKeys.
func TestNoBareNewError(t *testing.T) {
	seen := map[string]int{}
	walkCore(t, func(rel string, file *ast.File) {
		if ast.IsGenerated(file) {
			return
		}
		name := ""
		for _, im := range file.Imports {
			if v, _ := strconv.Unquote(im.Path.Value); v == "connectrpc.com/connect" {
				name = "connect"
				if im.Name != nil {
					name = im.Name.Name
				}
			}
		}
		if name == "" {
			return
		}
		for _, decl := range file.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				s, ok := n.(*ast.SelectorExpr)
				if !ok || s.Sel.Name != "NewError" || fmtIdent(s.X) != name {
					return true
				}
				if where := declName(decl); slices.Contains(bareNewError[rel], where) {
					seen[rel+" "+where]++
				} else {
					t.Errorf("%s: connect.NewError in %s: build it with errkeys.Connect (or add a sentinel or mapping helper to bareNewError)", rel, where)
				}
				return true
			})
		}
	})
	for f, decls := range bareNewError {
		for _, d := range decls {
			if seen[f+" "+d] == 0 {
				t.Errorf("%s: %s no longer calls connect.NewError: the check no longer sees it, drop it from bareNewError", f, d)
			}
		}
	}
}

// walkCore parses each of core's non-test files outside this package and hands it to fn, with its path from core/.
func walkCore(t *testing.T, fn func(rel string, file *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")
	self, _ := filepath.Abs(".")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if dir, _ := filepath.Abs(filepath.Dir(p)); dir == self {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel), file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// declName is a top-level declaration's name: a func's, or a var's first.
func declName(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		for _, sp := range d.Specs {
			if vs, ok := sp.(*ast.ValueSpec); ok {
				return vs.Names[0].Name
			}
		}
	}
	return ""
}

// TestRewriteWire: errkeys.Connect with KeyInternal, which replaced core's connect.NewError sites, sends the bytes the
// fallback sent for connect.NewError (status, body: code, message, the one KeyInternal detail), and its error still
// matches its cause with errors.Is.
func TestRewriteWire(t *testing.T) {
	cause := errors.New("no such row")
	for _, tc := range []struct {
		name string
		code connect.Code
		err  error
	}{
		{"plain", connect.CodeNotFound, cause},
		{"wrapped", connect.CodeInternal, fmt.Errorf("loading: %w", cause)},
		{"connect inside", connect.CodeUnavailable, connect.NewError(connect.CodeAborted, cause)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serve := func(err error) (int, string) {
				h := connect.NewUnaryHandler("/test.v1.T/Get", func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
					return nil, err
				}, WithInterceptors())
				req := httptest.NewRequest(http.MethodPost, "/test.v1.T/Get", strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code, rec.Body.String()
			}
			ce := Connect(tc.code, KeyInternal, tc.err)
			wantCode, wantBody := serve(connect.NewError(tc.code, tc.err))
			if code, body := serve(ce); code != wantCode || body != wantBody {
				t.Errorf("got %d %s, want %d %s", code, body, wantCode, wantBody)
			}
			if !errors.Is(ce, cause) || LocationOf(ce) == "" {
				t.Errorf("errors.Is %v, location %q", errors.Is(ce, cause), LocationOf(ce))
			}
		})
	}
}
