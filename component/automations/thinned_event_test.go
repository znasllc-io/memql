package automations

import (
	"bytes"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// A thinned copy is a row too large to broadcast, heard without its largest
// fields (component/node/eventbridge_thin.go). The node that wrote the row fires
// from the whole event. Firing on the copy too would run the automation twice,
// because the cluster guard's claim key hashes the event's payload and the two
// payloads differ, so the proof here is that the copy never reaches the args
// binding at all: a copy missing a required arg would otherwise be refused.
func TestScheduler_AThinnedCopyNeverFires(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)

	auto := &Automation{
		Name:    "deployOnBigRow",
		Args:    deployArgsSchema(),
		Trigger: &TriggerConfig{Event: "graph.node.updated.v1:ops:deploy"},
		Steps:   []*Step{{ID: "gate", Type: StepTypeFunction}},
	}
	s.automations[auto.Name] = auto
	if err := s.subscribeToEventTrigger(auto); err != nil {
		t.Fatalf("subscribeToEventTrigger: %v", err)
	}

	before := refusedFires.CountFor(auto.Name)
	thin := events.NewEvent("graph.node.updated.v1:ops:deploy", events.KindNodeUpdated, map[string]any{
		"id":                           "v1:ops:deploy:d1",
		events.PayloadOmittedFieldsKey: []any{"environment"},
	})
	thin.OriginNodeId = "the-node-that-wrote-it"
	bus.PublishSync(thin)
	if got := refusedFires.CountFor(auto.Name) - before; got != 0 {
		t.Errorf("a thinned copy reached the args binding (refused %d times); it must not fire at all", got)
	}

	// The control: the same payload without the marker is what binding refuses,
	// so the check above is the marker's doing.
	before = refusedFires.CountFor(auto.Name)
	bus.PublishSync(events.NewEvent("graph.node.updated.v1:ops:deploy", events.KindNodeUpdated, map[string]any{
		"id": "v1:ops:deploy:d1",
	}))
	if got := refusedFires.CountFor(auto.Name) - before; got != 1 {
		t.Errorf("the control event was refused %d times, want 1", got)
	}
}
