// Command kubectl-shim is installed into a thot session pod as `kubectl`.
//
// The pod holds no Kubernetes credentials at all (docs/adr/0037) — this
// forwards argv to thot-executor, which is the only process in the fleet
// that does. To the agent it looks and behaves like kubectl, so it writes
// ordinary bash and canUseTool gates it exactly like any other Bash call.
//
// A Go binary rather than a shell script wrapping grpcurl: the response
// carries kubectl's raw stdout, which routinely contains quotes and
// newlines, and parsing that back out of JSON with sed is a bug waiting
// to happen. This uses the generated client and copies bytes through.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func main() {
	addr := os.Getenv("EXECUTOR_ADDR")
	if addr == "" {
		fmt.Fprintln(os.Stderr, "kubectl: EXECUTOR_ADDR unset — this pod has no cluster access configured")
		os.Exit(2)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl: cannot reach executor: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if tok := os.Getenv("THOT_AUTH_TOKEN"); tok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}

	// os.Args[1:] verbatim — the executor spawns this array directly, so
	// metacharacters stay inert the whole way. Nothing is ever joined
	// into a string.
	//
	// ReadOnly is false: every call arriving here came from the agent's
	// Bash tool, which a human already approved through canUseTool. The
	// unattended read path is the separate kubectl_read MCP tool.
	resp, err := agentfleetv1.NewExecutorServiceClient(conn).Exec(ctx, &agentfleetv1.ExecRequest{
		Args:     os.Args[1:],
		ReadOnly: false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl: executor call failed: %v\n", err)
		os.Exit(2)
	}
	if refused := resp.GetRefused(); refused != "" {
		fmt.Fprintf(os.Stderr, "kubectl: refused by executor: %s\n", refused)
		os.Exit(2)
	}

	fmt.Print(resp.GetStdout())
	fmt.Fprint(os.Stderr, resp.GetStderr())
	os.Exit(int(resp.GetExitCode()))
}
