package steps

// event_cause_test.go -- the publish/event step stamps the run's cause on
// what it publishes, when its context carries one (memql#5382, Task 2 of
// epic memql#5380). A later automation triggered by the published event
// derives its own place in the chain from the triggering event's cause, so
// every publisher a run reaches must forward it -- this is the step half;
// the graph-write half is component/memql/graph_write_cause_test.go.
//
// The executor's own lifecycle/precondition events (executor.go's
// publishEvent / emitPreconditionMiss) get the identical change but no
// dedicated cause test here: nothing yet puts a cause onto a running
// automation's context -- that is a LATER task (the executor stamping its
// own run's cause into ctx before steps run). Today they are exercised only
// as plumbing, by the package's existing test suite continuing to pass with
// the signature change (ctx threaded through, same call sites).

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// TestEventStepStampsTheContextCause: a step run under a context carrying a
// cause publishes an event carrying that same cause -- driven through the
// REAL EventExecutor (v1_test.go's TestV1EventStepEvaluatesTopicAndPayload
// setup), with only the context varied.
func TestEventStepStampsTheContextCause(t *testing.T) {
	a := prepareV1(t, `{"name":"e","steps":[
		{"id":"pub","type":"event","event":{"topic":"app.done","payload":{"who":"args.who"}}}]}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"who": "w-1"})
	bus := events.NewBus()
	defer bus.Close()

	captured := make(chan events.Event, 1)
	unsubscribe := bus.Subscribe("app.done", func(e events.Event) { captured <- e })
	defer unsubscribe()

	cause := events.Cause{
		CausationId:   "run-1",
		CorrelationId: "corr-1",
		Depth:         2,
		Chain: []events.Link{
			{Automation: "a", RunId: "run-0"},
			{Automation: "b", RunId: "run-1"},
		},
	}
	ctx := events.ContextWithCause(context.Background(), cause)

	if _, err := (&EventExecutor{}).Execute(ctx, a.Steps[0], &Context{Evaluator: ev, EventBus: bus}); err != nil {
		t.Fatalf("event: %v", err)
	}

	select {
	case got := <-captured:
		if !reflect.DeepEqual(got.Cause, cause) {
			t.Fatalf("Cause = %#v, want %#v", got.Cause, cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event step published nothing")
	}
}

// TestEventStepWithNoContextCausePublishesARootEvent: absent a cause on ctx,
// the published event's Cause is the zero value -- a root event, exactly as
// today, never a leftover from an earlier call sharing the test process.
func TestEventStepWithNoContextCausePublishesARootEvent(t *testing.T) {
	a := prepareV1(t, `{"name":"e","steps":[
		{"id":"pub","type":"event","event":{"topic":"app.done","payload":{"who":"args.who"}}}]}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"who": "w-1"})
	bus := events.NewBus()
	defer bus.Close()

	captured := make(chan events.Event, 1)
	unsubscribe := bus.Subscribe("app.done", func(e events.Event) { captured <- e })
	defer unsubscribe()

	if _, err := (&EventExecutor{}).Execute(context.Background(), a.Steps[0], &Context{Evaluator: ev, EventBus: bus}); err != nil {
		t.Fatalf("event: %v", err)
	}

	select {
	case got := <-captured:
		if !got.Cause.IsZero() {
			t.Fatalf("Cause = %#v, want the zero value", got.Cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event step published nothing")
	}
}
