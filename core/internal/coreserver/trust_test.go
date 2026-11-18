package coreserver

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Every CoreService method is tagged B, C or neither, on purpose (docs/adr/0086 §1), and no tag names a method
// that does not exist.
func TestMethodTagsCoverCoreService(t *testing.T) {
	svc := agentfleetv1.File_agentfleet_v1_core_proto.Services().ByName("CoreService")
	seen := map[string]bool{}
	for i := 0; i < svc.Methods().Len(); i++ {
		full := "/agentfleet.v1.CoreService/" + string(svc.Methods().Get(i).Name())
		seen[full] = true
		if _, ok := methodTags[full]; !ok {
			t.Errorf("%s has no tag: decide B, C or neither", full)
		}
	}
	for m, letter := range methodTags {
		if !seen[m] {
			t.Errorf("tag for %s, which is no method", m)
		}
		if letter != "" && letter != "B" && letter != "C" && letter != "A|C" {
			t.Errorf("%s tagged %q", m, letter)
		}
	}
}

// Refusal per letter: a set missing the method's letter gets PermissionDenied keyed agent.rule_of_two; a set holding
// it, or a method tagged neither, passes.
func TestAuth_RuleOfTwoRefusesAMethodOutsideTheSet(t *testing.T) {
	const (
		b = "/agentfleet.v1.CoreService/ViewLogs"     // B
		c = "/agentfleet.v1.CoreService/Expose"       // C
		q = "/agentfleet.v1.CoreService/QueryMetrics" // B
		n = "/agentfleet.v1.CoreService/SendMessage"  // neither
	)
	for _, tc := range []struct {
		set, method string
		refused     bool
	}{
		{"BC", b, false}, {"BC", c, false}, {"BC", n, false},
		{"AB", b, false}, {"AB", c, true}, {"AB", n, false},
		{"AC", c, false}, {"AC", b, true}, {"AC", n, false},
		{"AC", q, true}, {"AB", q, false}, {"ABC", b, false}, {"ABC", c, false},
	} {
		a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1", trustSet: tc.set}, "provtoken")
		var req any = &agentfleetv1.SendMessageRequest{SessionId: "s1", From: "agent"}
		switch tc.method {
		case b:
			req = &agentfleetv1.ViewLogsRequest{}
		case q:
			req = &agentfleetv1.CoreQueryMetricsRequest{SessionId: "s1"}
		case c:
			req = &agentfleetv1.ExposeRequest{SessionId: "s1"}
		}
		err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), tc.method, req)
		if !tc.refused {
			if err != nil {
				t.Errorf("%s %s: %v", tc.set, tc.method, err)
			}
			continue
		}
		if codeOf(err) != codes.PermissionDenied || errkeys.Of(err) != errkeys.KeyRuleOfTwo || !strings.Contains(err.Error(), "card") {
			t.Errorf("%s %s: %v key %q, want PermissionDenied agent.rule_of_two naming the card path", tc.set, tc.method, err, errkeys.Of(err))
		}
	}
}

// The provisioner is no session: it has no set, and the rule never applies to it.
func TestAuth_RuleOfTwoLeavesTheProvisionerAlone(t *testing.T) {
	a := NewAuthenticator(fakeLeases{}, "provtoken")
	if err := callUnary(t, a, ctxWith(mdProvisionerToken, "provtoken"), "/agentfleet.v1.CoreService/AppendJournal", &agentfleetv1.AppendJournalRequest{}); err != nil {
		t.Errorf("provisioner refused: %v", err)
	}
}

// ADR-0086 §5: a set holding A may write a journal row (quarantined) or memory (quarantine repo), a set with C but no A
// writes plain, and B alone is refused. Core stamps quarantined from the stored set, whatever the pod claimed.
func TestAuth_AppendJournalQuarantineIsCoreDerived(t *testing.T) {
	const m = "/agentfleet.v1.CoreService/AppendJournal"
	for _, tc := range []struct {
		set     string
		claimed bool
		want    bool
		refused bool
	}{
		{"AB", false, true, false}, {"AC", false, true, false}, {"ABC", false, true, false},
		{"BC", true, false, false}, {"C", true, false, false}, {"B", false, false, true},
	} {
		a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1", trustSet: tc.set}, "provtoken")
		req := &agentfleetv1.AppendJournalRequest{Quarantined: tc.claimed}
		err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), m, req)
		if (err != nil) != tc.refused || (err != nil && errkeys.Of(err) != errkeys.KeyRuleOfTwo) {
			t.Errorf("%s: err %v, refused want %v", tc.set, err, tc.refused)
		}
		if !tc.refused && req.Quarantined != tc.want {
			t.Errorf("%s: quarantined %v, want %v", tc.set, req.Quarantined, tc.want)
		}
	}
}

// docs/adr/0084 §6: SpawnWorker and OpenIssue are effects (C), ListResponderCards is a read of the owner's alert
// history (B), WaitForSpawn is neither. An A·B session cannot spawn or open an issue; an A·C one cannot read cards.
func TestAuth_ReachMethodsFollowTheirTags(t *testing.T) {
	for _, tc := range []struct {
		set, method string
		req         any
		refused     bool
	}{
		{"AB", "SpawnWorker", &agentfleetv1.SpawnWorkerRequest{SessionId: "s1"}, true},
		{"AB", "OpenIssue", &agentfleetv1.OpenIssueRequest{SessionId: "s1"}, true},
		{"AC", "ListResponderCards", &agentfleetv1.ListResponderCardsRequest{SessionId: "s1"}, true},
		{"AC", "SpawnWorker", &agentfleetv1.SpawnWorkerRequest{SessionId: "s1"}, false},
		{"AB", "ListResponderCards", &agentfleetv1.ListResponderCardsRequest{SessionId: "s1"}, false},
		{"AB", "WaitForSpawn", &agentfleetv1.WaitForSpawnRequest{SessionId: "s1"}, false},
		// ADR-0084 §7: Speak is an effect (C) too.
		{"AB", "Speak", &agentfleetv1.SpeakRequest{SessionId: "s1"}, true},
		{"AC", "Speak", &agentfleetv1.SpeakRequest{SessionId: "s1"}, false},
	} {
		a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1", trustSet: tc.set, definition: "ops"}, "provtoken")
		err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), "/agentfleet.v1.CoreService/"+tc.method, tc.req)
		if got := codeOf(err) == codes.PermissionDenied && errkeys.Of(err) == errkeys.KeyRuleOfTwo; got != tc.refused || (!tc.refused && err != nil) {
			t.Errorf("%s %s: %v, refused want %v", tc.set, tc.method, err, tc.refused)
		}
	}
}

// ADR-0086 §2: AuthorizeKubeRead is a read of B, so a set without B is refused at the interceptor.
func TestAuth_AuthorizeKubeReadNeedsB(t *testing.T) {
	const m = "/agentfleet.v1.CoreService/AuthorizeKubeRead"
	for set, refused := range map[string]bool{"BC": false, "AB": false, "AC": true} {
		a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1", trustSet: set, definition: "ops"}, "provtoken")
		err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), m, &agentfleetv1.AuthorizeKubeReadRequest{SessionId: "s1"})
		if got := errkeys.Of(err) == errkeys.KeyRuleOfTwo; got != refused || (!refused && err != nil) {
			t.Errorf("%s: %v, want refused=%v", set, err, refused)
		}
	}
}
