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
	seen := map[string]bool{"/agentfleet.v1.ExecutorService/Exec": true}
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
		if letter != "" && letter != "B" && letter != "C" {
			t.Errorf("%s tagged %q", m, letter)
		}
	}
}

// Refusal per letter: a set missing the method's letter gets PermissionDenied keyed agent.rule_of_two; a set holding
// it, or a method tagged neither, passes.
func TestAuth_RuleOfTwoRefusesAMethodOutsideTheSet(t *testing.T) {
	const (
		b = "/agentfleet.v1.CoreService/ViewLogs"      // B
		c = "/agentfleet.v1.CoreService/AppendJournal" // C
		n = "/agentfleet.v1.CoreService/SendMessage"   // neither
	)
	for _, tc := range []struct {
		set, method string
		refused     bool
	}{
		{"BC", b, false}, {"BC", c, false}, {"BC", n, false},
		{"AB", b, false}, {"AB", c, true}, {"AB", n, false},
		{"AC", c, false}, {"AC", b, true}, {"AC", n, false},
		{"ABC", b, false}, {"ABC", c, false},
	} {
		a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1", trustSet: tc.set}, "provtoken")
		var req any = &agentfleetv1.SendMessageRequest{SessionId: "s1", From: "agent"}
		switch tc.method {
		case b:
			req = &agentfleetv1.ViewLogsRequest{}
		case c:
			req = &agentfleetv1.AppendJournalRequest{}
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
