package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWorkerPodLoss(t *testing.T) {
	cases := []struct {
		name string
		pod  corev1.PodStatus
		want string
	}{
		{"evicted reason", corev1.PodStatus{Reason: "Evicted"}, "Evicted"},
		{"preemption condition", corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "PreemptionByScheduler"}}}, "Preempted"},
		{"eviction condition", corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "EvictionByEvictionAPI"}}}, "Evicted"},
		{"plain failure", corev1.PodStatus{Reason: "Error"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient()
			_, err := c.Core.CoreV1().Pods(c.Namespace).Create(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{"job-name": "worker-1"}}, Status: tc.pod}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := c.WorkerPodLoss(context.Background(), "worker-1"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if got := c.WorkerPodLoss(context.Background(), "worker-other"); got != "" {
				t.Fatalf("another job's pod: %q", got)
			}
		})
	}
}
