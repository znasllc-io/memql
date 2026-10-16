package workbench

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type capacityRecorder struct {
	memql.IntegrationEngineAccess
	events chan memql.WorkEvent
}

func (r capacityRecorder) RecordWorkProgress(ctx context.Context, event memql.WorkEvent) error {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.RunId != "run" || run.OwnerUserId != "owner" {
		panic("lost owned progress across replica")
	}
	r.events <- event
	return nil
}

func TestCapacityWaitIsObservableAndCancellationEndsIt(t *testing.T) {
	gate := newCapacityGate(1, 2, func() (bool, error) { return false, nil })
	release, err := gate.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	recorder := capacityRecorder{events: make(chan memql.WorkEvent, 2)}
	i := &Integration{capacity: gate, engine: recorder}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := i.acquireHostCapacity(ctx, "run", "owner"); done <- err }()
	var waiting memql.WorkEvent
	select {
	case waiting = <-recorder.events:
	case <-time.After(2 * time.Second):
		t.Fatal("queue remained invisible")
	}
	if waiting.Phase != "queued" {
		t.Fatalf("not waiting: %+v", waiting)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request was admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue did not cancel")
	}
	final := <-recorder.events
	if final.ID != waiting.ID || final.Phase != "failed" {
		t.Fatalf("waiting state did not end: %+v", final)
	}
	waitCapacityQueue(t, gate, 0)
}
