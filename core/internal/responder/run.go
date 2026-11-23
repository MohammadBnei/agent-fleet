package responder

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
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

// Start invokes def for the batch's group and files its result in the background. The key is the definition, the
// alert group and the cooldown window it starts (docs/adr/0070 §1): a retry of a full fleet is the same run, the next
// window is a new one, and two responders on one group are two runs.
func (r *Runner) Start(def string, g Group, batch []json.RawMessage) {
	key := "alert:" + g.Key + ":" + def + ":" + time.Now().UTC().Format("20060102T150405")
	run := func() { r.run(def, g, key, batch) }
	if r.Wait != nil {
		r.Wait(run)
		return
	}
	go run()
}

// noThread: no run exists to answer. Nothing is filed: the alert reaches Discord and the bridge log only
// (docs/adr/0087), so this is loud and counted.
func (r *Runner) noThread(def string, g Group, why string, err error) {
	slog.Error("responder: "+why+", the alert reaches no session", "definition", def, "group", g.Key, "error", err)
	metrics.ResponderStartFailed.WithLabelValues(def).Inc()
}

// noResult: the run ended without a usable answer, so core files a card saying so. The owner always hears.
func (r *Runner) noResult(def, thread string, g Group, why string) {
	slog.Warn("responder: "+why+", filing a card", "definition", def, "thread", thread, "group", g.Key)
	if err := r.Filer.Unanswered(r.Ctx, thread, g); err != nil {
		slog.Error("responder: filing the card failed", "definition", def, "thread", thread, "group", g.Key, "error", err)
	}
}

func (r *Runner) run(def string, g Group, key string, batch []json.RawMessage) {
	ctx, cancel := context.WithTimeout(r.Ctx, RunLife)
	defer cancel()
	actx, err := r.Agents.AsClient(ctx, r.Client)
	if err != nil {
		r.noThread(def, g, "no client", err)
		return
	}
	input := Input(g, batch)
	var thread string
	for try := 0; ; try++ {
		res, err := r.Agents.InvokeUntrusted(actx, connect.NewRequest(&agentfleetv1.InvokeRequest{Definition: def, Input: input, IdempotencyKey: key}))
		if err == nil {
			thread = res.Msg.GetThreadId()
			break
		}
		if c := connect.CodeOf(err); (c != connect.CodeResourceExhausted && c != connect.CodeAborted) || try >= InvokeTries {
			r.noThread(def, g, "invoke failed", err)
			return
		}
		select {
		case <-ctx.Done():
			if r.Ctx.Err() != nil { // core is shutting down: a rollout is not a failed start
				slog.Info("responder: core is shutting down before the run could start", "definition", def, "group", g.Key)
				return
			}
			r.noThread(def, g, "the fleet had no room before the run's life ended", ctx.Err())
			return
		case <-time.After(InvokeRetry):
		}
	}
	for {
		res, err := r.Agents.WaitForResult(actx, connect.NewRequest(&agentfleetv1.WaitForResultRequest{ThreadId: thread, WaitSeconds: 60}))
		if err != nil {
			r.noResult(def, thread, g, "wait failed: "+err.Error())
			return
		}
		switch res.Msg.GetState() {
		case "running", "waiting":
			if ctx.Err() != nil {
				r.noResult(def, thread, g, "the run outlived RunLife")
				return
			}
			continue
		case "done":
		default:
			r.noResult(def, thread, g, "run ended "+res.Msg.GetState())
			return
		}
		head, ok := r.Defs.MayOpen(r.Client, def)
		if !ok {
			r.noResult(def, thread, g, "definition gone before filing")
			return
		}
		switch err := r.Filer.File(actx, def, head.Effective, thread, g, res.Msg.GetStructuredOutput()); {
		case errors.Is(err, ErrOutput):
			r.noResult(def, thread, g, "invalid structured output")
		case err != nil:
			slog.Error("responder: filing failed", "definition", def, "thread", thread, "group", g.Key, "error", err)
		}
		return
	}
}
