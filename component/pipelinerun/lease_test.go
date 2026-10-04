package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"
)

// lease_test.go -- which replica drives a run, and how a stranded run is
// taken up again (plan decision 10).

func TestWhoMayClaimARun(t *testing.T) {
	fresh, stale := testNow.Add(-30*time.Second), testNow.Add(-leaseStaleAfter)
	cases := []struct {
		name string
		run  Run
		want bool
	}{
		{"queued, nobody's", Run{Status: StatusQueued}, true},
		{"another replica's, renewed", Run{Status: StatusInProgress, DriverNodeID: "agent-b", DriverHeartbeatAt: fresh}, false},
		{"another replica's, stale", Run{Status: StatusInProgress, DriverNodeID: "agent-b", DriverHeartbeatAt: stale}, true},
		{"another replica's, never renewed", Run{Status: StatusQueued, DriverNodeID: "agent-b"}, true},
		{"this replica's previous process", Run{Status: StatusInProgress, DriverNodeID: "agent-a", DriverHeartbeatAt: fresh}, true},
		{"finished", Run{Status: StatusCompleted}, false},
	}
	for _, c := range cases {
		if got := claimable(c.run, "agent-a", testNow); got != c.want {
			t.Errorf("%s: claimable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAClaimIsAGatedFreshReadAndOneWrite(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)
	d := h.integ.snapshot()

	claimed, ok, err := h.integ.claim(context.Background(), d, run.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if claimed.DriverNodeID != "agent-a" || !claimed.DriverHeartbeatAt.Equal(testNow) {
		t.Errorf("the claimed row = %+v", claimed)
	}
	got, _ := h.store.run(run.ID)
	if got.DriverNodeID != "agent-a" || !got.DriverHeartbeatAt.Equal(testNow) || got.Status != StatusQueued {
		t.Errorf("the lease is written and nothing else: %+v", got)
	}
	if keys := h.gate.seen(); len(keys) != 1 || keys[0] != RunGateKey(run.ID) {
		t.Errorf("the claim is taken under the run's gate: %v", keys)
	}

	// Another replica, while the lease is fresh, takes nothing.
	b := New(Deps{Store: h.store, Gate: h.gate.run, NodeID: "agent-b", Now: func() time.Time { return testNow.Add(time.Minute) }})
	if _, ok, err := b.claim(context.Background(), b.snapshot(), run.ID); err != nil || ok {
		t.Errorf("a fresh lease is not taken: %v %v", ok, err)
	}
	// ...and once it is stale, takes it.
	late := New(Deps{Store: h.store, Gate: h.gate.run, NodeID: "agent-b", Now: func() time.Time { return testNow.Add(leaseStaleAfter) }})
	if _, ok, err := late.claim(context.Background(), late.snapshot(), run.ID); err != nil || !ok {
		t.Errorf("a stale lease is taken: %v %v", ok, err)
	}
	if got, _ := h.store.run(run.ID); got.DriverNodeID != "agent-b" {
		t.Errorf("the run is agent-b's now: %q", got.DriverNodeID)
	}
}

func TestAClaimFailsClosedWithNoGate(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)
	h.gate.refuse = errors.New("no database")
	if _, ok, err := h.integ.claim(context.Background(), h.integ.snapshot(), run.ID); err == nil || ok {
		t.Errorf("a claim with no gate claims nothing: %v %v", ok, err)
	}
	if got, _ := h.store.run(run.ID); got.DriverNodeID != "" {
		t.Errorf("nothing was written: %q", got.DriverNodeID)
	}
}

func TestWhatRecoveryTakesUp(t *testing.T) {
	cases := []struct {
		name string
		run  Run
		want bool
	}{
		{"queued 10 s ago: still its event's", Run{Status: StatusQueued, QueuedAt: testNow.Add(-10 * time.Second)}, false},
		{"queued 30 s ago, nobody claimed it", Run{Status: StatusQueued, QueuedAt: testNow.Add(-30 * time.Second)}, true},
		{"in progress with nobody holding it", Run{Status: StatusInProgress, QueuedAt: testNow}, true},
		{"driven, renewed a minute ago", Run{Status: StatusInProgress, DriverNodeID: "agent-z", DriverHeartbeatAt: testNow.Add(-time.Minute)}, false},
		{"driven, silent for three minutes", Run{Status: StatusInProgress, DriverNodeID: "agent-z", DriverHeartbeatAt: testNow.Add(-3 * time.Minute)}, true},
		{"claimed and queued, silent", Run{Status: StatusQueued, DriverNodeID: "agent-z", DriverHeartbeatAt: testNow.Add(-3 * time.Minute)}, true},
		{"this replica's previous process", Run{Status: StatusInProgress, DriverNodeID: "agent-a", DriverHeartbeatAt: testNow}, true},
		{"completed", Run{Status: StatusCompleted, QueuedAt: testNow.Add(-time.Hour)}, false},
	}
	for _, c := range cases {
		if got := recoverable(c.run, "agent-a", testNow); got != c.want {
			t.Errorf("%s: recoverable = %v, want %v", c.name, got, c.want)
		}
	}
}

// A run nobody claimed is driven by the poll's recovery, which EnableDriver
// installs as the poll's hook.
func TestThePollRecoversARunNobodyClaimed(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	run := dh.openRun(t, prOpening())
	dh.store.mu.Lock()
	r := dh.store.runs[run.ID]
	r.QueuedAt = testNow.Add(-time.Minute)
	dh.store.runs[run.ID] = r
	dh.store.mu.Unlock()

	if dh.integ.snapshot().Recover == nil || dh.integ.snapshot().SignalCancel == nil {
		t.Fatalf("EnableDriver installs the poll's recovery and the cancel signal")
	}
	res, err := dh.integ.Poll(automationCtx())
	if err != nil || res.RecoverError != "" {
		t.Fatalf("poll: %+v %v", res, err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(run.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-a" {
		t.Errorf("recovered and driven: %s/%s by %q", got.Status, got.Conclusion, got.DriverNodeID)
	}
}

func TestSignalCancelReachesOnlyARunDrivenHere(t *testing.T) {
	h := newHarness(t)
	l := h.integ.reserve("r1")
	defer h.integ.release(l)
	h.integ.SignalCancel(context.Background(), "v1:pipelines:run:r2")
	if l.isCancelled() {
		t.Errorf("another run's cancel does not reach this drive")
	}
	h.integ.SignalCancel(context.Background(), "v1:pipelines:run:r1")
	if !l.isCancelled() {
		t.Errorf("this run's cancel does, by either spelling of its id")
	}
	if h.integ.reserve("r1") != nil {
		t.Errorf("a run driven here is not reserved twice")
	}
}

func TestALostLeaseIsNeverCancelled(t *testing.T) {
	l := newLease("r1")
	l.lose()
	l.cancel()
	if l.isCancelled() || !l.isLost() {
		t.Errorf("a lease already lost is not this node's to cancel")
	}
	select {
	case <-l.stop:
	default:
		t.Errorf("a lost lease stops the drive")
	}
}
