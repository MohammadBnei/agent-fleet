package errkeys

import (
	"context"
	"errors"
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
	path, h := agentfleetv1connect.NewAgentServiceHandler(agentfleetv1connect.UnimplementedAgentServiceHandler{}, connect.WithInterceptors(ConnectInterceptor()))
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

// TestErrKeys: every key a call to Connect or Status names is one of errkeys.go's constants, named directly; no Key
// is written outside errkeys.go (no conversion, no new constant). Syntactic, over core's non-test files.
func TestErrKeys(t *testing.T) {
	keys := map[string]bool{}
	f, err := parser.ParseFile(token.NewFileSet(), "errkeys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					keys[n.Name] = true
				}
			}
		}
	}
	checked := 0
	err = filepath.WalkDir(filepath.Join("..", ".."), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || filepath.Base(filepath.Dir(p)) == "errkeys" {
			return err
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
		if name == "" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			s, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := s.X.(*ast.Ident); !ok || id.Name != name {
				return true
			}
			if s.Sel.Name == "Key" {
				t.Errorf("%s: errkeys.Key named outside errkeys.go", p)
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || (s.Sel.Name != "Connect" && s.Sel.Name != "Status") {
				return true
			}
			if id, ok := s.X.(*ast.Ident); !ok || id.Name != name {
				return true
			}
			checked++
			k, ok := c.Args[1].(*ast.SelectorExpr)
			if !ok || fmtIdent(k.X) != name || !keys[k.Sel.Name] {
				t.Errorf("%s: errkeys.%s with a key that is not one of errkeys.go's constants", p, s.Sel.Name)
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

// TestWiring: core's every Connect handler has the fallback outermost, and the gRPC server first in both chains.
func TestWiring(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "core", "run.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if n := strings.Count(s, "[]connect.Interceptor{errkeys.ConnectInterceptor(), "); n != 2 {
		t.Errorf("%d member/dashboard interceptor lists start with the fallback, want 2", n)
	}
	for _, want := range []string{"connect.WithInterceptors(\n\t\terrkeys.ConnectInterceptor(), ", "grpc.ChainUnaryInterceptor(errkeys.UnaryServerInterceptor(), ", "grpc.ChainStreamInterceptor(errkeys.StreamServerInterceptor(), "} {
		if !strings.Contains(s, want) {
			t.Errorf("run.go: no %q", want)
		}
	}
}
