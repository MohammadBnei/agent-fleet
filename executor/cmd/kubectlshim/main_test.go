package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The test binary is main when FLEET_TEST_RUN_MAIN is set, so the shim can be seen as a process does: the exit code,
// what it printed, and what the executor it dialled received.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runMain(t *testing.T, args []string, env ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append([]string{"FLEET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH"), "GOCOVERDIR=" + t.TempDir()}, env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), o.String(), e.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, o.String(), e.String()
}

type recorder struct {
	agentfleetv1.UnimplementedExecutorServiceServer
	mu   sync.Mutex
	auth []string
	args []string
}

func (r *recorder) Exec(ctx context.Context, req *agentfleetv1.ExecRequest) (*agentfleetv1.ExecResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	r.auth, r.args = md.Get("authorization"), req.GetArgs()
	r.mu.Unlock()
	return &agentfleetv1.ExecResponse{Stdout: "out\n", Stderr: "err\n", ExitCode: 3}, nil
}

func TestMain_Shim(t *testing.T) {
	// No EXECUTOR_ADDR: the pod has no cluster access, and says so, with exit 2.
	for _, env := range [][]string{nil, {"EXECUTOR_ADDR="}} {
		code, out, errOut := runMain(t, []string{"get", "pods"}, env...)
		if code != 2 || out != "" || errOut != "kubectl: EXECUTOR_ADDR unset — this pod has no cluster access configured\n" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", env, code, out, errOut)
		}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	srv := grpc.NewServer()
	agentfleetv1.RegisterExecutorServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	// The address and the token reach the call, the argv goes through verbatim, and the executor's answer is the shim's.
	code, out, errOut := runMain(t, []string{"get", "pods", "-n", "a b"}, "EXECUTOR_ADDR="+lis.Addr().String(), "THOT_AUTH_TOKEN=tok")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if code != 3 || out != "out\n" || errOut != "err\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if strings.Join(rec.auth, "|") != "Bearer tok" || strings.Join(rec.args, "|") != "get|pods|-n|a b" {
		t.Errorf("the executor received auth %q, args %q", rec.auth, rec.args)
	}

	// No token: no authorization header at all.
	rec.mu.Unlock()
	code, _, _ = runMain(t, []string{"x"}, "EXECUTOR_ADDR="+lis.Addr().String())
	rec.mu.Lock()
	if code != 3 || len(rec.auth) != 0 {
		t.Errorf("no token: exit %d, auth %q", code, rec.auth)
	}
}
