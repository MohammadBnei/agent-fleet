package sessions

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

const turnStall = 90 * time.Second

func TestDeriveLiveState(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-10 * time.Second)
	longAgo := now.Add(-10 * time.Minute)
	live := ptr("POD_PHASE_RUNNING")

	cases := []struct {
		name string
		task Session
		want LiveState
	}{
		{
			name: "no pod means liveness does not apply",
			task: Session{PodPhase: ptr("POD_PHASE_SUCCEEDED"), ActivitySeen: true, LastEntryType: ptr("result")},
			want: LiveStateNone,
		},
		{
			// The whole point of the taxonomy: this one must never be
			// reported as anything else, because nothing will happen
			// until a human clicks.
			name: "awaiting a human wins over every timing signal",
			task: Session{PodPhase: live, PendingDecisions: 1, ActivitySeen: true, LastActiveAt: &longAgo, LastEntryType: ptr("permission_request")},
			want: LiveStateBlocked,
		},
		{
			name: "a pod that has never spoken is unknown, not working",
			task: Session{PodPhase: live, ActivitySeen: false, LastActiveAt: &recent},
			want: LiveStateUnknown,
		},
		{
			name: "mid-turn agent output is working",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &recent, LastEntryType: ptr("assistant"), LastEntryFrom: ptr("agent")},
			want: LiveStateWorking,
		},
		{
			// A long tool call keeps appending entries, so last_active_at
			// moves on its own — the agent still owing a response is what
			// distinguishes stalled from slow.
			name: "a long-running agent turn is working, not stalled",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &longAgo, LastEntryType: ptr("assistant"), LastEntryFrom: ptr("agent")},
			want: LiveStateWorking,
		},
		{
			name: "a human message with no reply past the threshold is stalled",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &longAgo, LastEntryType: ptr("discussion"), LastEntryFrom: ptr("human")},
			want: LiveStateStalled,
		},
		{
			name: "the same message inside the threshold is still working",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &recent, LastEntryType: ptr("discussion"), LastEntryFrom: ptr("human")},
			want: LiveStateWorking,
		},
		{
			// An agent-authored discussion entry is the agent talking, not
			// the agent owing a reply — only `from` separates the two.
			name: "an agent discussion entry never counts as owing a reply",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &longAgo, LastEntryType: ptr("discussion"), LastEntryFrom: ptr("agent")},
			want: LiveStateWorking,
		},
		{
			name: "an answered question restarts the turn and can stall too",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &longAgo, LastEntryType: ptr("answer"), LastEntryFrom: ptr("human")},
			want: LiveStateStalled,
		},
		{
			name: "a finished turn nobody has looked at is done",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &recent, LastEntryType: ptr("result"), LastEntryFrom: ptr("agent")},
			want: LiveStateDone,
		},
		{
			name: "a finished turn seen since is idle",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &longAgo, SeenAt: &recent, LastEntryType: ptr("result"), LastEntryFrom: ptr("agent")},
			want: LiveStateIdle,
		},
		{
			// Opened, then more work finished afterwards — it is unseen
			// again, otherwise one early open would permanently suppress
			// the badge for that session.
			name: "work finishing after the last look is done again",
			task: Session{PodPhase: live, ActivitySeen: true, LastActiveAt: &recent, SeenAt: &longAgo, LastEntryType: ptr("result"), LastEntryFrom: ptr("agent")},
			want: LiveStateDone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveLiveState(&tc.task, now, turnStall); got != tc.want {
				t.Errorf("DeriveLiveState = %q, want %q", got, tc.want)
			}
		})
	}
}

// A pod that is still being created has no activity yet by definition;
// reporting it as anything but unknown would make every freshly dispatched
// task briefly look stalled or working.
func TestDeriveLiveStateAcrossLivePodPhases(t *testing.T) {
	now := time.Now()
	for _, phase := range []string{"POD_PHASE_PROVISIONING", "POD_PHASE_CREATED", "POD_PHASE_SCHEDULED", "POD_PHASE_RUNNING"} {
		task := Session{PodPhase: &phase}
		if got := DeriveLiveState(&task, now, turnStall); got != LiveStateUnknown {
			t.Errorf("phase %s: got %q, want %q", phase, got, LiveStateUnknown)
		}
	}
	for _, phase := range []string{"POD_PHASE_SUCCEEDED", "POD_PHASE_CRASHED", "POD_PHASE_UNSPECIFIED"} {
		task := Session{PodPhase: &phase, ActivitySeen: true}
		if got := DeriveLiveState(&task, now, turnStall); got != LiveStateNone {
			t.Errorf("phase %s: got %q, want %q", phase, got, LiveStateNone)
		}
	}
}
