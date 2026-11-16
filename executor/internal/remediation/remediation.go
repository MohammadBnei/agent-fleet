// Package remediation runs one approved catalogue action (ADR-0086 §3).
//
// The remediation executor pulls a single-use row by id from core, runs the
// action once under its own ServiceAccount, and reports the outcome and the
// inverse. Core already checked schema, rate limits and the denylist; the
// checks here are the ones only the cluster can answer: the object exists,
// the owner kind and crash-loop state, the replica bounds, selfHeal, and that a
// sync has something to do and nothing running.
//
// Deferred: ADR-0086 §3's node-count check. No catalogue action here can
// remove a node, and the executor holds no nodes grant; add it with the first
// action that needs it.
//
// The argv shapes are fixed here. Nothing from the row reaches kubectl except
// as a validated name in a slot, so the row cannot choose a verb.
package remediation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/MohammadBnei/agent-fleet/executor/internal/kubectl"
)

// Catalogue actions (ADR-0086 §3). No cordon, no rollback.
const (
	ActionRestart   = "restart"
	ActionScale     = "scale"
	ActionDeletePod = "delete_pod"
	ActionSyncApp   = "sync_app"
)

// Row is a remediation as core serves it.
type Row struct {
	ID        string
	Action    string
	Kind      string // restart: deployment or statefulset; scale: deployment
	Namespace string // empty for sync_app (Applications live in argocdNS)
	Name      string
	// Replicas, with MinReplicas and MaxReplicas (the definition's bounds), for scale.
	Replicas, MinReplicas, MaxReplicas int
}

// Outcome is what the executor reports back; Inverse is the argv that undoes
// the action, empty when none is needed.
type Outcome struct {
	Status  string // "done" or "refused"
	Detail  string
	Inverse []string
}

// Source is core's remediations API, as the executor needs it. The endpoint is
// S05's; this is the narrow seam it plugs into. Pull must consume the row
// (single use), so a Job retry cannot run it twice; S05's core enforces that.
type Source interface {
	Pull(ctx context.Context, id string) (Row, error)
	Report(ctx context.Context, id string, o Outcome) error
}

// Runner runs kubectl; kubectl.Run is the real one.
type Runner func(ctx context.Context, args []string) kubectl.Result

const argocdNS = "argocd"

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Run pulls row id, executes it once and reports. A refusal is reported as an
// outcome, not an error: the row was handled.
func Run(ctx context.Context, src Source, run Runner, id string) error {
	row, err := src.Pull(ctx, id)
	if err != nil {
		return fmt.Errorf("pull remediation %s: %w", id, err)
	}
	o := execute(ctx, run, row)
	// Logged first: if Report fails the mutation has already run, and the
	// outcome and inverse must not be lost.
	slog.Info("remediation executed", "id", id, "action", row.Action, "status", o.Status, "detail", o.Detail, "inverse", o.Inverse)
	return src.Report(ctx, id, o)
}

func refuse(f string, a ...any) Outcome {
	return Outcome{Status: "refused", Detail: fmt.Sprintf(f, a...)}
}

func execute(ctx context.Context, run Runner, r Row) Outcome {
	if !dns1123.MatchString(r.Name) {
		return refuse("invalid target name")
	}
	// Every action but sync_app names its namespace; an empty one would
	// resolve to the executor's own.
	if r.Action != ActionSyncApp && !dns1123.MatchString(r.Namespace) {
		return refuse("invalid namespace")
	}
	switch r.Action {
	case ActionRestart:
		return restart(ctx, run, r)
	case ActionScale:
		return scale(ctx, run, r)
	case ActionDeletePod:
		return deletePod(ctx, run, r)
	case ActionSyncApp:
		return syncApp(ctx, run, r)
	}
	return refuse("action %q is not in the catalogue", r.Action)
}

var kinds = map[string]string{"deployment": "deployment", "statefulset": "statefulset"}

// get returns one jsonpath field of the target, or a refusal when it is absent.
func get(ctx context.Context, run Runner, path string, args ...string) (string, *Outcome) {
	res := run(ctx, append(append([]string{"get"}, args...), "-o", "jsonpath="+path))
	if res.ExitCode != 0 {
		o := refuse("target not found: %s", strings.TrimSpace(res.Stderr))
		return "", &o
	}
	return res.Stdout, nil
}

// do runs argv with a server-side dry-run first (when dry), then for real.
func do(ctx context.Context, run Runner, argv []string, dry bool) *Outcome {
	if dry {
		if res := run(ctx, append(append([]string{}, argv...), "--dry-run=server")); res.ExitCode != 0 {
			o := refuse("dry-run failed: %s", strings.TrimSpace(res.Stderr))
			return &o
		}
	}
	if res := run(ctx, argv); res.ExitCode != 0 {
		o := refuse("kubectl failed: %s", strings.TrimSpace(res.Stderr))
		return &o
	}
	return nil
}

// selfHealed reports whether the workload's Application would revert a manual
// change. ArgoCD 3.x tracks by the tracking-id annotation
// (<app>:<group>/<kind>:<ns>/<name>) and no longer sets the instance label, so
// the annotation comes first and the label is the fallback. With neither it
// fails closed: it cannot tell, so scale is refused.
func selfHealed(ctx context.Context, run Runner, kind string, r Row) (bool, *Outcome) {
	app, o := get(ctx, run, `{.metadata.annotations.argocd\.argoproj\.io/tracking-id}`, kind, r.Name, "-n", r.Namespace)
	if o != nil {
		return false, o
	}
	app, _, _ = strings.Cut(app, ":")
	if app == "" {
		if app, o = get(ctx, run, `{.metadata.labels.argocd\.argoproj\.io/instance}`, kind, r.Name, "-n", r.Namespace); o != nil {
			return false, o
		}
	}
	if app == "" {
		o := refuse("no Argo tracking info on the workload; cannot check selfHeal")
		return false, &o
	}
	if !dns1123.MatchString(app) {
		o := refuse("invalid application name")
		return false, &o
	}
	v, o := get(ctx, run, `{.spec.syncPolicy.automated.selfHeal}`, "application", app, "-n", argocdNS)
	return v == "true", o
}

func restart(ctx context.Context, run Runner, r Row) Outcome {
	kind, ok := kinds[r.Kind]
	if !ok {
		return refuse("restart takes a deployment or statefulset")
	}
	if _, o := get(ctx, run, `{.metadata.name}`, kind, r.Name, "-n", r.Namespace); o != nil {
		return *o
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339)}}}}})
	if o := do(ctx, run, []string{"patch", kind, r.Name, "-n", r.Namespace, "-p", string(patch)}, true); o != nil {
		return *o
	}
	return Outcome{Status: "done", Detail: "restarted"} // a rolling restart is its own undo
}

func scale(ctx context.Context, run Runner, r Row) Outcome {
	// deployments/scale is the only scale grant (ADR-0086 §3). It is get+update,
	// so scale must send --current-replicas: with it kubectl does Get+Update on
	// the scale subresource; without it, a PATCH the role does not grant. The
	// precondition also fails the scale if the count moved since we read it.
	kind := r.Kind
	if kind != "deployment" {
		return refuse("scale takes a deployment")
	}
	if r.Replicas < r.MinReplicas || r.Replicas > r.MaxReplicas {
		return refuse("replicas %d outside bounds %d..%d", r.Replicas, r.MinReplicas, r.MaxReplicas)
	}
	cur, o := get(ctx, run, `{.spec.replicas}`, kind, r.Name, "-n", r.Namespace)
	if o != nil {
		return *o
	}
	if sh, o := selfHealed(ctx, run, kind, r); o != nil {
		return *o
	} else if sh {
		return refuse("application has selfHeal; a scale would be reverted")
	}
	if o := do(ctx, run, []string{"scale", kind + "/" + r.Name, "-n", r.Namespace, "--current-replicas=" + cur, fmt.Sprintf("--replicas=%d", r.Replicas)}, true); o != nil {
		return *o
	}
	return Outcome{Status: "done", Detail: fmt.Sprintf("scaled from %s to %d", cur, r.Replicas),
		Inverse: []string{"scale", kind + "/" + r.Name, "-n", r.Namespace, "--replicas=" + cur}}
}

func deletePod(ctx context.Context, run Runner, r Row) Outcome {
	owner, o := get(ctx, run, `{.metadata.ownerReferences[0].kind}`, "pod", r.Name, "-n", r.Namespace)
	if o != nil {
		return *o
	}
	if owner != "ReplicaSet" && owner != "StatefulSet" {
		return refuse("pod owner %q is not a ReplicaSet or StatefulSet", owner)
	}
	waiting, o := get(ctx, run, `{.status.containerStatuses[*].state.waiting.reason}`, "pod", r.Name, "-n", r.Namespace)
	if o != nil {
		return *o
	}
	if !strings.Contains(waiting, "CrashLoopBackOff") {
		return refuse("pod is not crash-looping")
	}
	if o := do(ctx, run, []string{"delete", "pod", r.Name, "-n", r.Namespace}, false); o != nil {
		return *o
	}
	return Outcome{Status: "done", Detail: "deleted; the controller recreates it"}
}

func syncApp(ctx context.Context, run Runner, r Row) Outcome {
	if _, o := get(ctx, run, `{.metadata.name}`, "application", r.Name, "-n", argocdNS); o != nil {
		return *o
	}
	if phase, o := get(ctx, run, `{.status.operationState.phase}`, "application", r.Name, "-n", argocdNS); o != nil {
		return *o
	} else if phase == "Running" {
		return refuse("a sync is already running")
	}
	if st, o := get(ctx, run, `{.status.sync.status}`, "application", r.Name, "-n", argocdNS); o != nil {
		return *o
	} else if st != "OutOfSync" {
		return refuse("application is %q, not OutOfSync", st)
	}
	// No revision: git's current one. No prune: partial reversibility, by design.
	patch := `{"operation":{"sync":{"prune":false}}}`
	if o := do(ctx, run, []string{"patch", "application", r.Name, "-n", argocdNS, "--type", "merge", "-p", patch}, false); o != nil {
		return *o
	}
	return Outcome{Status: "done", Detail: "sync requested without prune"}
}
