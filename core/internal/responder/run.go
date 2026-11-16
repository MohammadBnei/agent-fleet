package responder

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Agents is agentsvc.Server.
type Agents interface {
	AsClient(ctx context.Context, client string) (context.Context, error)
	InvokeUntrusted(ctx context.Context, req *connect.Request[agentfleetv1.InvokeRequest]) (*connect.Response[agentfleetv1.InvokeResponse], error)
	WaitForResult(ctx context.Context, req *connect.Request[agentfleetv1.WaitForResultRequest]) (*connect.Response[agentfleetv1.WaitForResultResponse], error)
}

// Tunables; tests shrink them.
var (
	// RunLife bounds how long core waits on one run before it stops filing for it.
	RunLife = 30 * time.Minute
	// InvokeRetry spaces the retries of an Invoke the fleet had no room for (the same key, so one run).
	InvokeRetry = 30 * time.Second
	InvokeTries = 20
)

// Runner starts a responder run and files its output once it ends.
//
// ponytail: the wait is a goroutine per run, lost on a core restart (the run still ends, nothing is filed). Alertmanager
// repeats the group every 4h; a sessions.Loop sweep over ended responder runs if that is too slow.
type Runner struct {
	Ctx    context.Context // outlives any one event: the dispatcher's Run context
	Client string          // the in-process invoker, the bridge's dispatcher
	Agents Agents
	Defs   interface {
		MayOpen(client, definition string) (definitions.Head, bool)
	}
	Filer Filer
	Wait  func(wg func()) // tests run the goroutine inline; nil is `go`
}

// Start invokes def for the group and files its result in the background. The key is the alert group plus the
// cooldown window it starts (docs/adr/0070 §1: (dispatcher, alert:<groupKey>)): a retry of a full fleet is the same
// run, and the next window is a new one.
func (r *Runner) Start(def string, g Group, input string) {
	key := "alert:" + g.Key + ":" + time.Now().UTC().Format("20060102T150405")
	run := func() { r.run(def, g, key, input) }
	if r.Wait != nil {
		r.Wait(run)
		return
	}
	go run()
}

func (r *Runner) run(def string, g Group, key, input string) {
	ctx, cancel := context.WithTimeout(r.Ctx, RunLife)
	defer cancel()
	actx, err := r.Agents.AsClient(ctx, r.Client)
	if err != nil {
		slog.Error("responder: no client", "error", err)
		return
	}
	var thread string
	for try := 0; ; try++ {
		res, err := r.Agents.InvokeUntrusted(actx, connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: def, Input: input, IdempotencyKey: key}))
		if err == nil {
			thread = res.Msg.GetThreadId()
			break
		}
		if c := connect.CodeOf(err); (c != connect.CodeResourceExhausted && c != connect.CodeAborted) || try >= InvokeTries {
			slog.Error("responder: invoke failed", "definition", def, "group", g.Key, "error", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(InvokeRetry):
		}
	}
	for {
		res, err := r.Agents.WaitForResult(actx, connect.NewRequest(&agentfleetv1.WaitForResultRequest{ThreadId: thread, WaitSeconds: 60}))
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("responder: wait failed", "definition", def, "thread", thread, "error", err)
			}
			return
		}
		switch res.Msg.GetState() {
		case "running", "waiting":
			continue
		case "done":
		default:
			slog.Warn("responder: run ended without a result", "definition", def, "thread", thread, "state", res.Msg.GetState())
			return
		}
		head, ok := r.Defs.MayOpen(r.Client, def)
		if !ok {
			slog.Warn("responder: definition gone before filing", "definition", def, "thread", thread)
			return
		}
		if err := r.Filer.File(actx, def, head.Effective, thread, g, res.Msg.GetStructuredOutput()); err != nil {
			slog.Error("responder: filing failed", "definition", def, "thread", thread, "group", g.Key, "error", err)
		}
		return
	}
}
