package workjournal

import (
	"context"
	"testing"
	"time"
)

func TestBeginCarriesHeartbeatOnFirstVersion(t *testing.T) {
	engine := &countingEngine{}
	j, clock := newClockedJournal(engine)
	j.beat = time.Hour
	w := work()
	w.Steps = nil
	run, err := j.Begin(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer run.stopHeartbeat()
	args := onlyCallNamed(t, engine.calls, "createWorkRun")
	if got := args["heartbeatAt"]; got != clock.at.Format(time.RFC3339Nano) {
		t.Errorf("first version heartbeat = %v, want %s", got, clock.at)
	}
	if updates := callsNamed(engine.calls, "updateWorkRun"); len(updates) != 0 {
		t.Errorf("opening a run without steps wrote redundant versions: %v", updates)
	}
}
