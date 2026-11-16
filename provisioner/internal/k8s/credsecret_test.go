package k8s

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func credSecret(t *testing.T, c *Client, id string) (found bool) {
	t.Helper()
	_, err := c.Core.CoreV1().Secrets("agent-fleet").Get(context.Background(), CredSecretName(id), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// ADR-0086 §6: the Secret is created before the Job and owned by it after.
func TestCredSecret_CreatedBeforeTheJobAndAdoptedAfter(t *testing.T) {
	c := newTestClient()
	var order []string
	c.Core.(*fake.Clientset).PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		order = append(order, a.GetResource().Resource)
		return false, nil, nil
	})
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "l"}); err != nil {
		t.Fatal(err)
	}
	var si, ji = -1, -1
	for i, r := range order {
		if r == "secrets" {
			si = i
		}
		if r == "jobs" {
			ji = i
		}
	}
	if si < 0 || ji < 0 || si > ji {
		t.Errorf("create order %v: the Secret must exist before the Job", order)
	}
	sec, _ := c.Core.CoreV1().Secrets("agent-fleet").Get(context.Background(), CredSecretName("task-1"), metav1.GetOptions{})
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" || sec.OwnerReferences[0].Name != WorkerResourceName("task-1") {
		t.Errorf("owner references %v", sec.OwnerReferences)
	}
}

// A refused Job leaves no credential behind.
func TestCredSecret_DeletedWhenTheJobCreateFails(t *testing.T) {
	c := newTestClient()
	c.Core.(*fake.Clientset).PrependReactor("create", "jobs", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission refused")
	})
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "l"}); err == nil {
		t.Fatal("want the Job's error")
	}
	if credSecret(t, c, "task-1") {
		t.Error("the Secret outlived a Job that was never created")
	}
}

// A Secret that cannot be adopted would outlive its Job: the warm fails and
// leaves neither.
func TestCredSecret_FailedAdoptionUndoesTheWarm(t *testing.T) {
	c := newTestClient()
	c.Core.(*fake.Clientset).PrependReactor("patch", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if err := c.CreateWorkerPod(context.Background(), WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "l"}); err == nil {
		t.Fatal("want the adoption error")
	}
	if credSecret(t, c, "task-1") {
		t.Error("the Secret outlived the warm")
	}
	if _, exists, _ := c.GetWorkerJob(context.Background(), "task-1"); exists {
		t.Error("the Job outlived the warm")
	}
}

// Teardown revokes through the Job's annotation, then deletes the Job: the
// Secret goes with it.
func TestCredSecret_DeleteWorkerJobRemovesIt(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "l"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteWorkerJob(ctx, "task-1"); err != nil {
		t.Fatal(err)
	}
	if credSecret(t, c, "task-1") {
		t.Error("the Secret survived DeleteWorkerJob")
	}
}

// A re-warm finds the previous warm's Secret still there (its Job's delete is
// asynchronous): it is replaced with the new lease and drops the dead owner.
func TestCredSecret_ReplacesAStaleOne(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Core.BatchV1().Jobs("agent-fleet").Delete(ctx, WorkerResourceName("task-1"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.createCredSecret(ctx, "task-1", map[string]string{"lease": "new"}); err != nil {
		t.Fatal(err)
	}
	sec, _ := c.Core.CoreV1().Secrets("agent-fleet").Get(ctx, CredSecretName("task-1"), metav1.GetOptions{})
	if sec.StringData["lease"] != "new" || len(sec.OwnerReferences) != 0 {
		t.Errorf("secret %+v: want the new lease and no stale owner", sec)
	}
}

// A second warm of a live session changes nothing: the running pod keeps its
// Secret (lease, owner) and its Job.
func TestCredSecret_SecondWarmLeavesALivePodAlone(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateWorkerPod(ctx, WorkerPodSpec{SessionID: "task-1", Repo: "r", LeaseID: "second"}); err == nil {
		t.Fatal("want the second warm refused")
	}
	sec, err := c.Core.CoreV1().Secrets("agent-fleet").Get(ctx, CredSecretName("task-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the live pod's Secret is gone: %v", err)
	}
	if sec.StringData["lease"] != "first" || len(sec.OwnerReferences) != 1 {
		t.Errorf("secret %+v: want the first lease and its owner", sec)
	}
	if _, exists, _ := c.GetWorkerJob(ctx, "task-1"); !exists {
		t.Error("the live Job was deleted")
	}
}
