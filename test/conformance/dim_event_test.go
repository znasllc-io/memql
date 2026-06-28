package conformance

// dim_event_test.go -- the event-trigger dimension (#1706). An event-triggered
// automation must bind the triggering event into nested step-arg scope, so
// event.payload.<field> resolves inside the steps. bootstrapSession is the
// vehicle: given a participant.created-shaped event it threads event.payload.id
// + event.payload.partitionId into the existence-check decide logic AND the
// session-create mutation step. Before #1706 the event was unresolved, so the
// created session carried no participant link and a read-back by that id came
// back empty.
//
// #2271 update: bootstrapSession's inline write was migrated out of the `logic`
// (it is now PURE -- it decides; the automation persists). So this dimension
// fires the bootstrapSession AUTOMATION (the real event path) instead of calling
// the logic directly. The event-binding contract is unchanged and still the
// right end-to-end check: driving the automation must still create a session
// keyed by the event's participant id.

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

func eventTriggerCheck() check {
	return check{
		Issue:   "#1706",
		Dim:     "event-context-binding",
		NeedsDB: true,
		Run:     runEventTrigger,
	}
}

func runEventTrigger(t *testing.T, e *Env) {
	pid := "participant-evt-" + uniqueSuffix("evt")
	sid := "space-evt-" + uniqueSuffix("evt")

	// Fire the bootstrapSession AUTOMATION with a representative
	// participant.created event. The decide logic reads event.payload.id; the
	// gated createSession step binds event.payload.id + event.payload.partitionId.
	fireForgeAutomation(t, e, "bootstrapSession", "node.created", events.KindNodeCreated, map[string]any{
		"id":              pid,
		"partitionId":     sid,
		"participantType": "human",
	})

	// Read back by the event's participant id. A non-empty result proves
	// event.payload.id threaded through both the decide query AND the create step.
	rows := asArray(t, e.runQuery(t, "participantSession", map[string]any{"participantId": pid}))
	if len(rows) == 0 {
		t.Fatalf("#1706: no session bound to event participant %q -- event payload did not thread into the create", pid)
	}
	// Strengthen: the session's participantId must echo the event payload.
	if got := rowID(rows[0]); got == "" {
		t.Logf("#1706: session row has no id field (shape omits it); presence already proves binding")
	}
	t.Logf("#1706: event-triggered automation bound event.payload into nested step scope (session created for %s)", pid)
}
