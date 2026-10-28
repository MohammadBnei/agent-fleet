package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The test binary is main when FLEET_TEST_RUN_MAIN is set, so the refusals can be seen as a process does: the
// exit code and what it logged.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runMain(t *testing.T, env ...string) (code int, out string) {
	t.Helper()
	// A main that does not refuse would serve for ever: the test fails instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append([]string{"FLEET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH")}, env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), b.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, b.String()
}

// SESSION_ID is refused before LEASE_ID, the level's warning comes first, and LEASE_ID's refusal already carries
// the session id.
func TestMain_Refusals(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		want []string // substrings, in this order
		not  string   // a substring that must be absent
	}{
		{"nothing", nil, []string{`"SESSION_ID is required"`}, "LEASE_ID"},
		{"blank ids", []string{"SESSION_ID=", "LEASE_ID="}, []string{`"SESSION_ID is required"`}, "LEASE_ID"},
		{"lease only", []string{"LEASE_ID=l"}, []string{`"SESSION_ID is required"`}, "sessionId"},
		{"session only", []string{"SESSION_ID=s1"}, []string{`"LEASE_ID is required: without it every CoreService call is rejected","sessionId":"s1"`}, "SESSION_ID is required"},
		{"bad level warns first", []string{"LOG_LEVEL=bogus"}, []string{`"invalid LOG_LEVEL, defaulting to info","value":"bogus"`, `"SESSION_ID is required"`}, ""},
		{"a vault proxy without a token", []string{"SESSION_ID=s1", "LEASE_ID=l1", "FLEET_VAULT_PROXY=http://v:1"}, []string{`"egress router","sessionId":"s1","error":"egress: FLEET_VAULT_PROXY is not a proxy URL with a token"`}, ""},
		{"an egress port that is no port", []string{"SESSION_ID=s1", "LEASE_ID=l1", "FLEET_VAULT_PROXY=http://t:p@v:1", "EGRESS_PORT=x"}, []string{`"egress router listen"`, "unknown port"}, ""},
	} {
		code, out := runMain(t, c.env...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1\n%s", c.name, code, out)
		}
		rest := out
		for _, w := range c.want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Errorf("%s: %q not in order in\n%s", c.name, w, out)
				break
			}
			rest = rest[i+len(w):]
		}
		if c.not != "" && strings.Contains(out, c.not) {
			t.Errorf("%s: %q should not be logged\n%s", c.name, c.not, out)
		}
	}
}

// telemetryRecorder is a CoreService that remembers what the sidecar pushed.
type telemetryRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	got chan *agentfleetv1.PushToolTelemetryRequest
}

func (f *telemetryRecorder) PushToolTelemetry(_ context.Context, req *agentfleetv1.PushToolTelemetryRequest) (*agentfleetv1.PushToolTelemetryResponse, error) {
	select {
	case f.got <- req:
	default:
	}
	return &agentfleetv1.PushToolTelemetryResponse{}, nil
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// main wires every non-refusal field of the Config to its consumer: CORE_GRPC_ADDR is where it dials,
// WORKTREE_PATH the tree its telemetry reads, FLEET_VAULT_HOSTS which CONNECTs go to the vault proxy
// (FLEET_VAULT_PROXY), and the four ports what it listens on. Each is a value that differs from its default, so
// a field dropped on the way to its consumer is seen here and nowhere else.
func TestMain_Wiring(t *testing.T) {
	// A fake core, and a worktree with one changed file for its telemetry to find.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &telemetryRecorder{got: make(chan *agentfleetv1.PushToolTelemetryRequest, 1)}
	gs := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(gs, rec)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	tree := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", tree, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(tree, "wired.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "x")
	if err := os.WriteFile(filepath.Join(tree, "wired.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fake vault proxy: it records the CONNECT line it is sent and refuses it.
	vault, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	connect := make(chan string, 1)
	go func() {
		c, err := vault.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		line, _ := bufio.NewReader(c).ReadString('\n')
		connect <- strings.TrimSpace(line)
		_, _ = c.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
	}()

	ports := map[string]string{"MCP_PORT": freePort(t), "LOCAL_API_PORT": freePort(t), "HEALTH_PORT": freePort(t), "EGRESS_PORT": freePort(t)}
	env := []string{
		"FLEET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH"),
		"SESSION_ID=s-wired", "LEASE_ID=l-wired",
		"CORE_GRPC_ADDR=" + lis.Addr().String(),
		"WORKTREE_PATH=" + tree,
		"FLEET_VAULT_PROXY=http://tok:en@" + vault.Addr().String(),
		"FLEET_VAULT_HOSTS=wired.example.test",
	}
	for k, v := range ports {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = env
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	fail := func(format string, a ...any) {
		t.Helper()
		t.Fatalf(format+"\nlogs:\n%s", append(a, logs.String())...)
	}

	// CORE_GRPC_ADDR and WORKTREE_PATH: the first push to the fake core is the worktree's diff, as this session.
	select {
	case req := <-rec.got:
		if req.GetSessionId() != "s-wired" || !strings.Contains(req.GetSummaryJson(), `"wired.txt"`) {
			fail("telemetry %v", req)
		}
	case <-time.After(15 * time.Second):
		fail("the fake core got no telemetry: CORE_GRPC_ADDR or WORKTREE_PATH is not used")
	}

	// EGRESS_PORT and FLEET_VAULT_HOSTS: a CONNECT for a listed host reaches the vault proxy, tunnelled as asked.
	var egress net.Conn
	for deadline := time.Now().Add(10 * time.Second); ; {
		egress, err = net.Dial("tcp", "127.0.0.1:"+ports["EGRESS_PORT"])
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			fail("the egress router does not listen on EGRESS_PORT: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = egress.Close() }()
	_, _ = egress.Write([]byte("CONNECT wired.example.test:443 HTTP/1.1\r\nHost: wired.example.test:443\r\n\r\n"))
	select {
	case line := <-connect:
		if line != "CONNECT wired.example.test:443 HTTP/1.1" {
			fail("the vault proxy was sent %q", line)
		}
	case <-time.After(10 * time.Second):
		fail("FLEET_VAULT_HOSTS: a listed host did not go to the vault proxy")
	}

	// The three listeners are on their ports.
	for _, name := range []string{"MCP_PORT", "LOCAL_API_PORT", "HEALTH_PORT"} {
		var c net.Conn
		for deadline := time.Now().Add(10 * time.Second); ; {
			c, err = net.Dial("tcp", "127.0.0.1:"+ports[name])
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				fail("nothing listens on %s: %v", name, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
