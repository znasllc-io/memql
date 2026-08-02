package automations

// chain_head_clock_test.go -- memql#2941, carrying the memql#2823 / #2867
// lesson forward onto the key that is actually live.
//
// #2899 deleted the automation step cache and #2941 deleted its
// key-computation chain, which took fingerprint_clock_test.go with it. That
// test encoded a real lesson -- a wall-clock reading in a key is an accidental
// discriminator that makes the key unique every time -- and deleting it left
// the lesson as prose only.
//
// Prose is a weaker instrument than the test it replaced, and the hazard did
// not leave with the cache. It moved somewhere worse. On the cache path a
// clock in the key was merely SLOW (a guaranteed miss). On this path it is a
// CORRECTNESS failure:
//
//	events.Event.Timestamp (time.Now().UTC())
//	  -> eventFingerprintData   <- deliberately does NOT project it
//	    -> ComputeInitialChainHead
//	      -> executionDedup.isDuplicate       (per-process)
//	      -> clusterGuard.Claim               (CROSS-REPLICA)
//
// Add the timestamp to that projection and every execution gets a unique chain
// head, so both gates stop deduplicating and every event-triggered automation
// runs once per replica -- silently, in exactly the 2-replica topology CLAUDE.md
// mandates everywhere.
//
// The pre-existing TestComputeInitialChainHead_* tests cannot catch this: they
// pass the SAME map literal twice, so they stay green through a change that
// adds a clock field upstream. These build the projection from a real
// events.Event at two different instants, which is the only shape that fails.

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// TestEventFingerprintDataExcludesTheWallClock is the guard named in
// eventFingerprintData's doc comment.
func TestEventFingerprintDataExcludesTheWallClock(t *testing.T) {
	payload := map[string]any{"nodeId": "v1:cognition:utterance:abc"}

	early := &events.Event{
		Topic:     "graph.node.created",
		Kind:      events.KindNodeCreated,
		Payload:   payload,
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	later := &events.Event{
		Topic:     "graph.node.created",
		Kind:      events.KindNodeCreated,
		Payload:   payload,
		Timestamp: time.Date(2029, 6, 30, 12, 34, 56, 789, time.UTC),
	}

	gotEarly := eventFingerprintData(early)
	gotLater := eventFingerprintData(later)

	for key := range gotEarly {
		if key == "timestamp" || key == "time" || key == "at" {
			t.Errorf("eventFingerprintData projected %q. A wall clock in this map reaches "+
				"ComputeInitialChainHead, and from there execution dedup AND the "+
				"cross-replica cluster guard -- so every event-triggered automation would "+
				"run once per replica.", key)
		}
	}

	headEarly := ComputeInitialChainHead("someAutomation", "event", gotEarly, "")
	headLater := ComputeInitialChainHead("someAutomation", "event", gotLater, "")

	if headEarly != headLater {
		t.Errorf("the same event at two instants produced different chain heads:\n"+
			"  %s\n  %s\n"+
			"Dedup and clusterGuard.Claim both key on this value, so a time-varying "+
			"chain head means the same automation executes on every replica.",
			headEarly, headLater)
	}
}

// TestEventFingerprintDataStillDiscriminatesRealInputs is the other half, and
// the reason the exclusion must stay SURGICAL rather than becoming a recursive
// sweep for clock-shaped keys. `payload` legitimately carries business fields
// -- including ones named like timestamps -- and those MUST still discriminate.
// Dropping a real input from a key is a correctness bug; leaving a clock in one
// is merely a slow one.
func TestEventFingerprintDataStillDiscriminatesRealInputs(t *testing.T) {
	base := func(payload map[string]any) map[string]any {
		return eventFingerprintData(&events.Event{
			Topic:     "graph.node.created",
			Kind:      events.KindNodeCreated,
			Payload:   payload,
			Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	}

	head := func(d map[string]any) string {
		return ComputeInitialChainHead("someAutomation", "event", d, "")
	}

	a := head(base(map[string]any{"nodeId": "node-a"}))
	b := head(base(map[string]any{"nodeId": "node-b"}))
	if a == b {
		t.Error("two different payloads produced the same chain head; real inputs must discriminate")
	}

	// A business field that happens to be named "timestamp" is an input, not a
	// clock. A recursive strip would drop it and silently dedup distinct events.
	t1 := head(base(map[string]any{"nodeId": "n", "timestamp": "2026-01-01T00:00:00Z"}))
	t2 := head(base(map[string]any{"nodeId": "n", "timestamp": "2027-01-01T00:00:00Z"}))
	if t1 == t2 {
		t.Error("payload.timestamp was treated as a clock and stripped. It is a business " +
			"field: two events differing only in it are DIFFERENT events, and collapsing " +
			"them makes dedup discard a real execution.")
	}
}

// TestEventFingerprintDataHandlesNoEvent pins the schedule/manual path, where
// there is no triggering event at all.
func TestEventFingerprintDataHandlesNoEvent(t *testing.T) {
	if got := eventFingerprintData(nil); got != nil {
		t.Errorf("eventFingerprintData(nil) = %v, want nil", got)
	}
}
