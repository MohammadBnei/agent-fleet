package remediation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/executor/internal/kubectl"
)

// fake answers `get` by jsonpath substring and records every argv.
type fake struct {
	calls  [][]string
	gets   map[string]string // jsonpath substring -> stdout
	failOn string            // substring of joined argv that fails
}

func (f *fake) run(_ context.Context, args []string) kubectl.Result {
	f.calls = append(f.calls, args)
	j := strings.Join(args, " ")
	if f.failOn != "" && strings.Contains(j, f.failOn) {
		return kubectl.Result{ExitCode: 1, Stderr: "boom"}
	}
	if args[0] == "get" {
		for k, v := range f.gets {
			if strings.Contains(j, k) {
				return kubectl.Result{Stdout: v}
			}
		}
		return kubectl.Result{ExitCode: 1, Stderr: "not found"}
	}
	return kubectl.Result{}
}

func (f *fake) mutations() (n int) {
	for _, c := range f.calls {
		if c[0] != "get" && !strings.Contains(strings.Join(c, " "), "--dry-run") {
			n++
		}
	}
	return
}

func TestRestart(t *testing.T) {
	f := &fake{gets: map[string]string{"metadata.name": "core"}}
	o := execute(context.Background(), f.run, Row{Action: ActionRestart, Kind: "deployment", Namespace: "ns", Name: "core"})
	if o.Status != "done" || f.mutations() != 1 {
		t.Fatalf("%+v %v", o, f.calls)
	}
	if !strings.Contains(strings.Join(f.calls[1], " "), "--dry-run=server") {
		t.Error("dry-run must come first")
	}
}

func TestRestartDryRunFailureRunsNothing(t *testing.T) {
	f := &fake{gets: map[string]string{"metadata.name": "core"}, failOn: "--dry-run"}
	o := execute(context.Background(), f.run, Row{Action: ActionRestart, Kind: "deployment", Namespace: "ns", Name: "core"})
	if o.Status != "refused" || f.mutations() != 0 {
		t.Fatalf("%+v %v", o, f.calls)
	}
}

func TestScaleRecordsInverse(t *testing.T) {
	f := &fake{gets: map[string]string{"spec.replicas": "2", "instance": "", "metadata.labels": ""}}
	o := execute(context.Background(), f.run, Row{Action: ActionScale, Kind: "deployment", Namespace: "ns", Name: "web", Replicas: 4, MinReplicas: 1, MaxReplicas: 5})
	if o.Status != "done" || strings.Join(o.Inverse, " ") != "scale deployment/web -n ns --replicas=2" {
		t.Fatalf("%+v", o)
	}
}

func TestScaleBoundsAndSelfHeal(t *testing.T) {
	f := &fake{}
	o := execute(context.Background(), f.run, Row{Action: ActionScale, Kind: "deployment", Namespace: "ns", Name: "web", Replicas: 9, MinReplicas: 1, MaxReplicas: 5})
	if o.Status != "refused" || len(f.calls) != 0 {
		t.Fatalf("out of bounds: %+v", o)
	}
	f = &fake{gets: map[string]string{"spec.replicas": "2", "labels.argocd": "web", "selfHeal": "true"}}
	o = execute(context.Background(), f.run, Row{Action: ActionScale, Kind: "deployment", Namespace: "ns", Name: "web", Replicas: 3, MinReplicas: 1, MaxReplicas: 5})
	if o.Status != "refused" || f.mutations() != 0 {
		t.Fatalf("selfHeal: %+v %v", o, f.calls)
	}
}

func TestDeletePodOwner(t *testing.T) {
	for owner, want := range map[string]string{"ReplicaSet": "done", "StatefulSet": "done", "Job": "refused", "": "refused"} {
		f := &fake{gets: map[string]string{"ownerReferences": owner}}
		o := execute(context.Background(), f.run, Row{Action: ActionDeletePod, Namespace: "ns", Name: "p"})
		if o.Status != want {
			t.Errorf("owner %q: %+v", owner, o)
		}
		if want == "refused" && f.mutations() != 0 {
			t.Errorf("owner %q deleted", owner)
		}
	}
}

func TestSyncNeverPrunes(t *testing.T) {
	f := &fake{gets: map[string]string{"metadata.name": "app"}}
	o := execute(context.Background(), f.run, Row{Action: ActionSyncApp, Name: "app"})
	last := strings.Join(f.calls[len(f.calls)-1], " ")
	if o.Status != "done" || !strings.Contains(last, `"prune":false`) || !strings.Contains(last, "-n argocd") {
		t.Fatalf("%+v %s", o, last)
	}
}

// The row cannot choose a verb or smuggle a flag.
func TestRefusesUnknownActionAndBadNames(t *testing.T) {
	f := &fake{}
	for _, r := range []Row{
		{Action: "cordon", Name: "n1"},
		{Action: "exec", Name: "p"},
		{Action: ActionDeletePod, Namespace: "ns", Name: "--all"},
		{Action: ActionDeletePod, Namespace: "-A", Name: "p"},
		{Action: ActionRestart, Kind: "node", Namespace: "ns", Name: "n"},
	} {
		if o := execute(context.Background(), f.run, r); o.Status != "refused" {
			t.Errorf("%+v: %+v", r, o)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("kubectl ran: %v", f.calls)
	}
}

type src struct {
	row Row
	err error
	got *Outcome
}

func (s *src) Pull(context.Context, string) (Row, error) { return s.row, s.err }
func (s *src) Report(_ context.Context, _ string, o Outcome) error {
	s.got = &o
	return nil
}

func TestRunPullsExecutesReports(t *testing.T) {
	f := &fake{gets: map[string]string{"metadata.name": "app"}}
	s := &src{row: Row{Action: ActionSyncApp, Name: "app"}}
	if err := Run(context.Background(), s, f.run, "r1"); err != nil || s.got == nil || s.got.Status != "done" {
		t.Fatalf("%v %+v", err, s.got)
	}
	s = &src{err: errors.New("gone")}
	if err := Run(context.Background(), s, f.run, "r1"); err == nil || s.got != nil {
		t.Fatal("a failed pull must run and report nothing")
	}
}
