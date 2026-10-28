package errkeys

import (
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
	"os"
	"path/filepath"
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

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// TestConstructors: the code and words are the caller's; the key rides as the detail, and Of reads it back.
func TestConstructors(t *testing.T) {
	ce := Connect(connect.CodeFailedPrecondition, KeyDefinitionChanged, errors.New("DEFINITION_CHANGED: start a new thread"))
	if ce.Code() != connect.CodeFailedPrecondition || ce.Message() != "DEFINITION_CHANGED: start a new thread" || Of(ce) != KeyDefinitionChanged {
		t.Errorf("connect: %v %q", ce, Of(ce))
	}
	ge := Status(codes.PermissionDenied, KeyInternal, "nope")
	if s, _ := status.FromError(ge); s.Code() != codes.PermissionDenied || s.Message() != "nope" || Of(ge) != KeyInternal {
		t.Errorf("grpc: %v %q", ge, Of(ge))
	}
	if Of(errors.New("plain")) != "" || Of(nil) != "" || Of(connect.NewError(connect.CodeInternal, errors.New("x"))) != "" {
		t.Error("an unkeyed error has a key") // the positive control for Of
	}
}

// TestConnectFallback: a raw error and an unkeyed Connect error leave keyed internal with their code and words; a
// keyed one keeps its key (one detail); nil stays nil.
func TestConnectFallback(t *testing.T) {
	raw := withConnectKey(errors.New("send: broken pipe"))
	if Of(raw) != KeyInternal || connect.CodeOf(raw) != connect.CodeUnknown || !strings.Contains(raw.Error(), "send: broken pipe") {
		t.Errorf("raw: %v", raw)
	}
	bare := withConnectKey(connect.NewError(connect.CodeNotFound, errors.New("gone")))
	if Of(bare) != KeyInternal || connect.CodeOf(bare) != connect.CodeNotFound {
		t.Errorf("bare: %v", bare)
	}
	own := withConnectKey(Connect(connect.CodeAborted, KeyDefinitionChanged, errors.New("x")))
	var ce *connect.Error
	if Of(own) != KeyDefinitionChanged || !errors.As(own, &ce) || len(ce.Details()) != 1 {
		t.Errorf("own: %v", own)
	}
	if withConnectKey(nil) != nil {
		t.Error("nil became an error")
	}
	unary := keyAll{}.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, errors.New("u") })
	if _, err := unary(context.Background(), nil); Of(err) != KeyInternal {
		t.Errorf("unary: %v", err)
	}
	stream := keyAll{}.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error { return errors.New("s") })
	if err := stream(context.Background(), nil); Of(err) != KeyInternal {
		t.Errorf("stream: %v", err)
	}
}

// TestGRPCFallback: the same over gRPC, unary and streaming.
func TestGRPCFallback(t *testing.T) {
	unary := UnaryServerInterceptor()
	if _, err := unary(context.Background(), nil, nil, func(context.Context, any) (any, error) { return nil, status.Error(codes.NotFound, "gone") }); Of(err) != KeyInternal || status.Code(err) != codes.NotFound {
		t.Errorf("unary: %v", err)
	}
	stream := StreamServerInterceptor()
	if err := stream(nil, nil, nil, func(any, grpc.ServerStream) error { return errors.New("raw") }); Of(err) != KeyInternal || status.Code(err) != codes.Unknown {
		t.Errorf("stream: %v", err)
	}
	if _, err := unary(context.Background(), nil, nil, func(context.Context, any) (any, error) { return nil, Status(codes.Aborted, KeyDefinitionChanged, "x") }); Of(err) != KeyDefinitionChanged {
		t.Errorf("own: %v", err)
	}
	if _, err := unary(context.Background(), nil, nil, func(context.Context, any) (any, error) { return 1, nil }); err != nil {
		t.Errorf("nil: %v", err)
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
	go gs.Serve(lis)
	defer gs.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = agentfleetv1.NewCoreServiceClient(conn).GetSession(context.Background(), &agentfleetv1.GetSessionRequest{})
	if status.Code(err) != codes.Unimplemented || Of(err) != KeyInternal {
		t.Errorf("grpc: %v key %q", err, Of(err))
	}
}

// TestErrKeys: every key a call to Connect or Status names is one of errkeys.go's Key constants, named directly; no
// Key is written outside errkeys.go (no conversion, no new constant), Connect and Status are only ever called (never
// taken as a value, so an alias cannot carry a key past this check), and errkeys is never dot-imported. Syntactic,
// over core's non-test files outside this package.
func TestErrKeys(t *testing.T) {
	keys, values := map[string]bool{}, map[string]string{}
	f, err := parser.ParseFile(token.NewFileSet(), "errkeys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Key" {
					t.Errorf("errkeys.go: a constant %s that is not a Key", vs.Names[0].Name)
					continue
				}
				for i, n := range vs.Names {
					v := vs.Values[i].(*ast.BasicLit).Value
					if other, dup := values[v]; dup {
						t.Errorf("errkeys.go: %s and %s are both %s", other, n.Name, v)
					}
					keys[n.Name], values[v] = true, n.Name
				}
			}
		}
	}
	self, _ := filepath.Abs(".")
	checked := 0
	err = filepath.WalkDir(filepath.Join("..", ".."), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if dir, _ := filepath.Abs(filepath.Dir(p)); dir == self {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		name := ""
		for _, im := range file.Imports {
			if v, _ := strconv.Unquote(im.Path.Value); v == "github.com/MohammadBnei/agent-fleet/core/internal/errkeys" {
				name = "errkeys"
				if im.Name != nil {
					name = im.Name.Name
				}
			}
		}
		if name == "." {
			t.Errorf("%s: errkeys dot-imported", p)
			return nil
		}
		if name == "" {
			return nil
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
			switch {
			case s.Sel.Name == "Key":
				t.Errorf("%s: errkeys.Key named outside errkeys.go", p)
			case (s.Sel.Name == "Connect" || s.Sel.Name == "Status") && !called[s]:
				t.Errorf("%s: errkeys.%s taken as a value: call it, with its key named", p, s.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 2 || checked < 1 {
		t.Errorf("%d keys, %d calls checked: the check no longer sees them", len(keys), checked) // the positive control
	}
}

func fmtIdent(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// TestWiring: every Connect handler core builds (a generated New…Handler) takes its interceptors through
// errkeys.WithInterceptors, written at the call, and none through connect.WithInterceptors or WithHandlerOptions;
// every gRPC server's first options chain errkeys' interceptors first, and no other option adds interceptors.
// Syntactic, over core's non-test files.
func TestWiring(t *testing.T) {
	handlers, servers := 0, 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imp := map[string]string{} // local name → import path
		for _, im := range file.Imports {
			v, _ := strconv.Unquote(im.Path.Value)
			n := v[strings.LastIndex(v, "/")+1:]
			if im.Name != nil {
				n = im.Name.Name
			}
			imp[n] = v
		}
		sel := func(e ast.Expr) (string, string) { // the import path and name e selects, or ""
			s, ok := e.(*ast.SelectorExpr)
			if !ok {
				return "", ""
			}
			return imp[fmtIdent(s.X)], s.Sel.Name
		}
		calls := func(e ast.Expr, path, name string) bool {
			c, ok := e.(*ast.CallExpr)
			if !ok {
				return false
			}
			pp, n := sel(c.Fun)
			return pp == path && n == name
		}
		const ek = "github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
		chained := map[ast.Expr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			path, name := sel(c.Fun)
			switch {
			case strings.HasSuffix(path, "connect") && strings.Contains(path, "/proto/gen/") && strings.HasPrefix(name, "New") && strings.HasSuffix(name, "Handler"):
				handlers++
				n := 0
				for _, a := range c.Args[1:] {
					if calls(a, ek, "WithInterceptors") {
						n++
					}
					if calls(a, "connectrpc.com/connect", "WithInterceptors") || calls(a, "connectrpc.com/connect", "WithHandlerOptions") || c.Ellipsis.IsValid() {
						t.Errorf("%s: %s: interceptors not through errkeys.WithInterceptors", p, name)
					}
				}
				if n != 1 {
					t.Errorf("%s: %s with %d errkeys.WithInterceptors, want 1", p, name, n)
				}
			case path == "google.golang.org/grpc" && name == "NewServer":
				servers++
				if len(c.Args) < 2 || !calls(c.Args[0], "google.golang.org/grpc", "ChainUnaryInterceptor") || !calls(c.Args[1], "google.golang.org/grpc", "ChainStreamInterceptor") ||
					!calls(c.Args[0].(*ast.CallExpr).Args[0], ek, "UnaryServerInterceptor") || !calls(c.Args[1].(*ast.CallExpr).Args[0], ek, "StreamServerInterceptor") {
					t.Errorf("%s: grpc.NewServer does not open with errkeys' chains", p)
				} else {
					chained[c.Args[0]], chained[c.Args[1]] = true, true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || chained[c] {
				return true
			}
			if path, name := sel(c.Fun); path == "google.golang.org/grpc" && strings.HasSuffix(name, "Interceptor") {
				t.Errorf("%s: grpc.%s outside the server's opening chains", p, name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if handlers < 3 || servers < 1 {
		t.Errorf("%d Connect handlers, %d gRPC servers: the check no longer sees them", handlers, servers) // the positive control
	}
}

// TestContextCodes: a plain context error keeps the code each protocol sends for it unkeyed (Connect's toWire, gRPC's
// FromContextError), wrapped or not; any other plain error is Unknown.
func TestContextCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		cc   connect.Code
		gc   codes.Code
		name string
	}{
		{context.Canceled, connect.CodeCanceled, codes.Canceled, "canceled"},
		{context.DeadlineExceeded, connect.CodeDeadlineExceeded, codes.DeadlineExceeded, "deadline"},
		{fmt.Errorf("query: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, codes.DeadlineExceeded, "wrapped deadline"},
		{os.ErrDeadlineExceeded, connect.CodeDeadlineExceeded, codes.Unknown, "os deadline"},
		{errors.New("broken pipe"), connect.CodeUnknown, codes.Unknown, "plain"},
	} {
		if got := withConnectKey(c.err); connect.CodeOf(got) != c.cc || Of(got) != KeyInternal || !strings.Contains(got.Error(), c.err.Error()) {
			t.Errorf("%s: Connect %v", c.name, got)
		}
		if got := withStatusKey(c.err); status.Code(got) != c.gc || Of(got) != KeyInternal {
			t.Errorf("%s: gRPC %v", c.name, got)
		}
	}
}

// TestSentinelUnchanged: keying never changes the handler's error, which may be a shared sentinel: concurrent
// requests do not race on it (run with -race), and it stays unkeyed with its meta and details.
func TestSentinelUnchanged(t *testing.T) {
	sentinel := connect.NewError(connect.CodeNotFound, errors.New("no such thread"))
	sentinel.Meta().Set("X-Why", "gone")
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := withConnectKey(sentinel)
			var ce *connect.Error
			if Of(got) != KeyInternal || !errors.As(got, &ce) || ce.Code() != connect.CodeNotFound || ce.Message() != "no such thread" ||
				ce.Meta().Get("X-Why") != "gone" || len(ce.Details()) != 1 {
				t.Errorf("keyed: %v", got)
			}
		}()
	}
	wg.Wait()
	if Of(sentinel) != "" || len(sentinel.Details()) != 0 {
		t.Errorf("the sentinel was changed: %d details", len(sentinel.Details()))
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
