package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// The certification ladder's two event-fired automations (epic memql#5408)
// decide WHEN they fire in their @filter, and both decisions are load-bearing
// in a way no run of the body can show -- the automation corpus answers every
// call and never evaluates a filter:
//
//   - learnFromSucceededRun must fire on the TRANSITION to succeeded, once. A
//     run keeps being written after it succeeds (the retention sweep folds its
//     summary onto it), and each of those writes is a node.updated event; the
//     handler runs the shadow comparison, so firing on the state rather than
//     the transition would count one recording's match twice.
//   - onProcedurePromotionDecided must fire for a DECIDED procedurePromotion
//     and for nothing else -- not the approval being raised, and not any other
//     kind of gate, which the same topic carries.
//
// Driven through the path the scheduler fires through: bindEventArgs, then
// evaluateTriggerFilter, over an event shaped the way executeUpdate publishes
// one -- the stored payload flattened onto the envelope and kept whole under
// `payload`, and the PRIOR version's status as `oldStatus`, present only when
// that version had one.

func loadedProcedureAutomation(t *testing.T, name string) *Automation {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, a := range loaded {
		if a != nil && a.Name == name {
			if a.Trigger == nil || a.Trigger.Filter == "" {
				t.Fatalf("%s loaded with no trigger filter; the property under test is the filter", name)
			}
			return a
		}
	}
	t.Fatalf("no automation named %s loads from the embedded tree (%d loaded)", name, len(loaded))
	return nil
}

// graphUpdatedEvent builds the event executeUpdate publishes for one write.
func graphUpdatedEvent(concept, id string, payload map[string]any, oldStatus string) events.Event {
	flat := map[string]any{
		"id": id, "nodeId": id, "concept": concept, "nodeType": "node",
		"actor":   "system:journal",
		"payload": payload,
	}
	for k, v := range payload {
		flat[k] = v
	}
	if oldStatus != "" {
		flat["oldStatus"] = oldStatus
	}
	ev := events.NewEvent("graph.node.updated."+concept, events.KindNodeUpdated, flat)
	ev.Metadata = map[string]string{"actor": "system:journal"}
	return ev
}

func filterFires(t *testing.T, a *Automation, ev events.Event) bool {
	t.Helper()
	bound, _, err := bindEventArgs(a, &ev)
	if err != nil {
		t.Fatalf("%s: binding the event's args refused the fire: %v", a.Name, err)
	}
	fires, err := evaluateTriggerFilter(a, &ev, bound)
	if err != nil {
		t.Fatalf("%s: the filter refused to evaluate: %v", a.Name, err)
	}
	return fires
}

func TestLearnFromSucceededRunFiresOnTheTransitionToSucceededOnly(t *testing.T) {
	a := loadedProcedureAutomation(t, "learnFromSucceededRun")
	const run = "v1:work:run"
	for _, tc := range []struct {
		name      string
		status    string
		oldStatus string
		fires     bool
	}{
		{"running to succeeded", "succeeded", "running", true},
		{"waiting to succeeded", "succeeded", "waiting", true},
		// The prior version carried no status, so the engine publishes no
		// oldStatus at all: unset is not `succeeded`, and the run is learned.
		{"a first write that is already succeeded", "succeeded", "", true},
		// The shape the filter exists to refuse: a later write to a run that
		// had already succeeded -- the summary fold -- fires nothing.
		{"succeeded to succeeded", "succeeded", "succeeded", false},
		{"running to failed", "failed", "running", false},
		{"a heartbeat while running", "running", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := graphUpdatedEvent(run, run+":r1", map[string]any{"status": tc.status, "automationName": "x"}, tc.oldStatus)
			if got := filterFires(t, a, ev); got != tc.fires {
				t.Fatalf("learnFromSucceededRun fired=%v for %s, want %v", got, tc.name, tc.fires)
			}
		})
	}
}

func TestOnProcedurePromotionDecidedFiresOnlyForADecidedPromotion(t *testing.T) {
	a := loadedProcedureAutomation(t, "onProcedurePromotionDecided")
	const approval = "v1:work:approval"
	for _, tc := range []struct {
		name     string
		kind     string
		decision string
		fires    bool
	}{
		{"an approved promotion", "procedurePromotion", "approved", true},
		{"a rejected promotion", "procedurePromotion", "rejected", true},
		// Raised and still pending: createWorkApproval stamps decision "",
		// and an update that has not decided must not move the ladder.
		{"a pending promotion", "procedurePromotion", "", false},
		{"another kind of gate, decided", "sideEffect", "approved", false},
		{"a routing review, decided", "routingReview", "rejected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := graphUpdatedEvent(approval, approval+":a1", map[string]any{"kind": tc.kind, "decision": tc.decision}, "")
			if got := filterFires(t, a, ev); got != tc.fires {
				t.Fatalf("onProcedurePromotionDecided fired=%v for %s, want %v", got, tc.name, tc.fires)
			}
		})
	}
}
