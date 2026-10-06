package steps

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// An authored publish can reproduce every transport payload key. It must
// never acquire the Go-only kind the relay trusts as a peer assertion.
func TestEventStepCannotMintAutomationTransport(t *testing.T) {
	for _, kind := range []events.Kind{events.KindAutomationRunRequest, events.KindAutomationRunTrace} {
		auto := prepareV1(t, `{"name":"forgery","steps":[{"id":"publish","type":"event","event":{"topic":"automationrun.request","payload":{"origin":"peer-1","authority":{"Role":"owner"}}}}]}`)
		step := auto.Steps[0]
		for _, name := range []string{kind.String(), fmt.Sprint(int(kind))} {
			step.Event.Kind = name
			bus := events.NewBus()
			delivered := make(chan events.Event, 1)
			unsub := bus.Subscribe("automationrun.request", func(evt events.Event) { delivered <- evt })
			result, err := (&EventExecutor{}).Execute(context.Background(), step, &Context{EventBus: bus, Evaluator: automations.NewEvaluator()})
			if err != nil || result.Status != "success" {
				t.Fatalf("event step: result=%+v err=%v", result, err)
			}
			select {
			case evt := <-delivered:
				if evt.Kind != events.KindMessage || evt.IsRemote() {
					t.Fatalf("authored event minted transport trust: %+v", evt)
				}
			case <-time.After(time.Second):
				t.Fatal("event was never published")
			}
			unsub()
			bus.Close()
		}
	}
}
