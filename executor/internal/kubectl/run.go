package kubectl

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// maxOutput caps what we hand back — a `kubectl get pods -A -o yaml` on a
// busy cluster is megabytes, and it all ends up in an agent's context.
const maxOutput = 100_000

// DefaultTimeout bounds a single kubectl invocation. Without it a hung
// API call would pin an executor goroutine indefinitely.
const DefaultTimeout = 60 * time.Second

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes kubectl with args as a real argv array.
//
// exec.CommandContext takes the arguments as a slice and does not invoke a
// shell, so metacharacters are inert: `get pods; kubectl delete ns argocd`
// arrives at kubectl as one nonsense argument rather than two commands.
// That structural property is why the wire type is `repeated string` and
// not a command string.
func Run(ctx context.Context, args []string) Result {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			// Couldn't start, or the context deadline fired — surface it
			// as stderr rather than an empty success.
			code = -1
			stderr.WriteString(err.Error())
		}
	}

	return Result{
		Stdout:   clip(stdout.String()),
		Stderr:   clip(stderr.String()),
		ExitCode: code,
	}
}

func clip(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n… (truncated)"
}
