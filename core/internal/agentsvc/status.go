package agentsvc

import (
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// status is docs/adr/0066 §10's derived state, from what is already stored.
// ponytail: error carries no code yet; the codes need pod_reason, which the
// provisioner does not report until the client-listener PR.
func status(t *sessions.Session) *agentfleetv1.Status {
	if !sessions.IsPodPhaseLive(t.PodPhase) {
		// Never last_error itself: it is the provisioner's text (pod names,
		// images) and a shared thread's members are not admins.
		if t.LastError != nil && *t.LastError != "" {
			return &agentfleetv1.Status{State: "error", Message: "the session could not start"}
		}
		return &agentfleetv1.Status{State: "idle"}
	}
	switch sessions.DeriveLiveState(t, time.Now(), dashboard.DefaultTurnStall) {
	case sessions.LiveStateBlocked:
		return &agentfleetv1.Status{State: "waiting"}
	case sessions.LiveStateUnknown:
		return &agentfleetv1.Status{State: "loading"}
	case sessions.LiveStateWorking, sessions.LiveStateStalled:
		return &agentfleetv1.Status{State: "working"}
	}
	return &agentfleetv1.Status{State: "idle"}
}
