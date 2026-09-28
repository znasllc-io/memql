//go:build agent

package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// deniedAuditLogger is the identity side of workerservice.IdentityAuditor: what
// it receives is what createAuditEvent is asked to write.
type deniedAuditLogger struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (r *deniedAuditLogger) Log(_ context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

// TestDeniedDispatchAuditNamesNoCredential: a refused worker call is audited
// with the AGENT as the actor, and an agent is not a v1:identity:identity row.
// The actor used to be "agent:<agentId>" in actorIdentityId -- a relationship to
// v1:identity:identity -- so with a canonical agent id the value parsed as
// v1:agents:agent and every denial row (scope elevation, kill switch, policy,
// classifier) was refused at insert, leaving the security trail silent about
// exactly the calls it exists to record.
func TestDeniedDispatchAuditNamesNoCredential(t *testing.T) {
	const (
		agentId = "v1:agents:agent:a-audit"
		owner   = "v1:identity:user:u-audit"
	)
	rec := &deniedAuditLogger{}
	d := &Dispatcher{
		auditor: &workerservice.IdentityAuditor{AuditLogger: rec},
		clock:   func() time.Time { return time.Date(2026, 9, 28, 9, 50, 0, 0, time.UTC) },
	}
	req := Request{Tool: "workerHost", Action: "exec", AgentId: agentId, OwnerUserId: owner, CorrelationId: "call-1"}
	for _, outcome := range []string{"denied_by_scope", "kill_switch_engaged", "denied_by_policy", "denied_by_classifier"} {
		d.emitDenied(context.Background(), req, gateResult{deny: true, outcome: outcome, errorMessage: "refused"})
	}

	if len(rec.events) != 4 {
		t.Fatalf("audit events = %d, want one per denial path (4)", len(rec.events))
	}
	for _, ev := range rec.events {
		if ev.ActorIdentity != "" {
			concept, _, _ := id.ParseNodeId(ev.ActorIdentity)
			t.Errorf("%s: actorIdentity = %q (concept %q), want empty -- an agent presents no "+
				"v1:identity:identity credential, and the insert refuses any other concept there",
				ev.Action, ev.ActorIdentity, concept)
		}
		if ev.ActorUserId != owner {
			t.Errorf("%s: actorUserId = %q, want the owner the agent acted for (%q)", ev.Action, ev.ActorUserId, owner)
		}
		if got := ev.Detail["actor"]; got != "agent:"+agentId {
			t.Errorf("%s: detail.actor = %v, want %q -- which agent was refused must survive in the row",
				ev.Action, got, "agent:"+agentId)
		}
		switch ev.TargetType {
		case "agent":
			if ev.TargetId != agentId {
				t.Errorf("%s: targetId = %q, want the agent %q", ev.Action, ev.TargetId, agentId)
			}
		case "user":
			if ev.TargetId != owner {
				t.Errorf("%s: targetId = %q, want the owner %q", ev.Action, ev.TargetId, owner)
			}
		default:
			t.Errorf("%s: targetType = %q, want agent or user", ev.Action, ev.TargetType)
		}
	}
}
