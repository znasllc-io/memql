package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// capturingDispatcher records what it was asked to run.
type capturingDispatcher struct {
	mu   sync.Mutex
	reqs []DispatchRequest
}

func (d *capturingDispatcher) Dispatch(_ context.Context, req DispatchRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, req)
}

func (d *capturingDispatcher) seen() []DispatchRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DispatchRequest(nil), d.reqs...)
}

// stubClaimer answers a fixed verdict and records the keys it was asked about.
type stubClaimer struct {
	grant bool
	mu    sync.Mutex
	names []string
	keys  []string
	ttls  []time.Duration
}

func (c *stubClaimer) ClaimWithTTL(_ context.Context, name, key string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
	c.keys = append(c.keys, key)
	c.ttls = append(c.ttls, ttl)
	return c.grant
}

func runEvent(id, status, automation, owner string) events.Event {
	return events.Event{
		Topic: "graph.node.updated.v1:work:run",
		Payload: map[string]any{
			"id": id,
			"payload": map[string]any{
				"status":         status,
				"automationName": automation,
				"ownerUserId":    owner,
				"goalId":         "goal-1",
			},
		},
	}
}

// newDispatchProbe wires an integration with a dispatcher and a claimer, and
// dispatches SYNCHRONOUSLY so the assertions do not race the goroutine
// HandleRunEvent spawns.
func newDispatchProbe(t *testing.T, grant bool) (*Integration, *capturingDispatcher, *stubClaimer) {
	t.Helper()
	i := New(nil, nil)
	d := &capturingDispatcher{}
	c := &stubClaimer{grant: grant}
	i.SetDispatcher(d)
	i.SetRunClaimer(c)
	return i, d, c
}

// TestDispatchRefusesWithNoClaimer is the fail-CLOSED property, and it is the
// one place in this package where a nil seam refuses instead of degrading.
//
// An unclaimed dispatch on a two-replica agent deployment runs one run TWICE,
// and a run's steps have side effects -- the duplicate is a second email, a
// second file, a second charge. A run that does not start is visible; a run
// that ran twice is not.
func TestDispatchRefusesWithNoClaimer(t *testing.T) {
	i := New(nil, nil)
	d := &capturingDispatcher{}
	i.SetDispatcher(d)
	// deliberately no SetRunClaimer

	i.dispatchRun(context.Background(), DispatchRequest{RunId: "r1", Status: runStatusRunning, AutomationName: "demo"})

	if got := d.seen(); len(got) != 0 {
		t.Fatalf("dispatched %d run(s) with no cross-replica claim installed; it must refuse", len(got))
	}
}

// TestDispatchClaimsOnTheRunId pins the claim GRAIN. Keying on the automation
// name instead would let one run of a template block every other run of it.
func TestDispatchClaimsOnTheRunId(t *testing.T) {
	i, d, c := newDispatchProbe(t, true)

	i.dispatchRun(context.Background(), DispatchRequest{RunId: "run-42", Status: runStatusRunning, AutomationName: "analyzeFile"})

	if len(c.keys) != 1 || c.keys[0] != "run-42" {
		t.Fatalf("claim keys = %v, want exactly [run-42] -- the run id is the identity of the work", c.keys)
	}
	if c.names[0] == "analyzeFile" {
		t.Error("the claim was namespaced by the AUTOMATION name; one run of a template would then block every other run of it")
	}
	if c.ttls[0] <= 0 {
		t.Error("the claim was taken with no lease; a claimant that dies mid-run would leave a run nobody can ever retake")
	}
	if got := d.seen(); len(got) != 1 {
		t.Fatalf("dispatched %d, want 1", len(got))
	}
}

// TestDispatchYieldsToAPeerThatWonTheClaim: losing the claim means another
// replica has it, and this one must do nothing at all.
func TestDispatchYieldsToAPeerThatWonTheClaim(t *testing.T) {
	i, d, _ := newDispatchProbe(t, false)

	i.dispatchRun(context.Background(), DispatchRequest{RunId: "run-42", Status: runStatusRunning, AutomationName: "demo"})

	if got := d.seen(); len(got) != 0 {
		t.Fatalf("dispatched %d run(s) after LOSING the claim; that is the duplicate-execution bug", len(got))
	}
}

// TestRunEventFiltering pins which run events reach a dispatch at all.
//
// The statuses are not an arbitrary list. `waiting` is the sharp one: a parked
// run is silent on purpose, and the sweep flips it back to `running` when its
// timer is due -- THAT write is the event that dispatches it. Dispatching on
// `waiting` itself would run a run that is deliberately paused on a person.
func TestRunEventFiltering(t *testing.T) {
	cases := []struct {
		name       string
		ev         events.Event
		wantClaims int
	}{
		{"running with a template dispatches", runEvent("r1", "running", "demo", "u1"), 1},
		{"compiling does not", runEvent("r2", "compiling", "", "u1"), 0},
		{"waiting does not", runEvent("r3", "waiting", "demo", "u1"), 0},
		{"succeeded does not", runEvent("r4", "succeeded", "demo", "u1"), 0},
		{"failed does not", runEvent("r5", "failed", "demo", "u1"), 0},
		{"abandoned does not", runEvent("r6", "abandoned", "demo", "u1"), 0},
		{"running with no template does not", runEvent("r7", "running", "", "u1"), 0},
		{"an event with no id does not", events.Event{Payload: map[string]any{}}, 0},
		{"an event with no payload at all does not", events.Event{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, _, c := newDispatchProbe(t, true)
			i.HandleRunEvent(tc.ev)
			// HandleRunEvent dispatches on a goroutine; wait for it to settle.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				c.mu.Lock()
				n := len(c.keys)
				c.mu.Unlock()
				if n >= tc.wantClaims {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			c.mu.Lock()
			got := len(c.keys)
			c.mu.Unlock()
			if got != tc.wantClaims {
				t.Errorf("claims = %d, want %d", got, tc.wantClaims)
			}
		})
	}
}

// TestIdOnlyRunEventIsPassedThrough: an event carrying no row payload cannot
// be judged, so it is passed to the privileged read behind the seam rather
// than dropped. One wasted claim beats a class of run that never starts.
func TestIdOnlyRunEventIsPassedThrough(t *testing.T) {
	i, _, c := newDispatchProbe(t, true)

	i.HandleRunEvent(events.Event{Payload: map[string]any{"id": "run-x"}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.keys)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) != 1 || c.keys[0] != "run-x" {
		t.Fatalf("claims = %v, want [run-x]; an id-only event must reach the privileged read", c.keys)
	}
}

// TestARunEventOnANodeWithNoDispatcherDoesNothing: every bff and identity
// replica sees these events. They must not claim, log per event, or act.
func TestARunEventOnANodeWithNoDispatcherDoesNothing(t *testing.T) {
	i := New(nil, nil)
	c := &stubClaimer{grant: true}
	i.SetRunClaimer(c)
	// deliberately no SetDispatcher

	i.HandleRunEvent(runEvent("r1", "running", "demo", "u1"))
	time.Sleep(50 * time.Millisecond)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.keys) != 0 {
		t.Fatalf("a node with no dispatcher took %d claim(s); it must ignore run events entirely", len(c.keys))
	}
}

// TestClaimLeaseOutlivesTheAbandonWindow: the lease must exceed the window the
// abandoned sweep judges by, or a run is retaken while its first claimant is
// still, by the sweep's own definition, alive.
func TestClaimLeaseOutlivesTheAbandonWindow(t *testing.T) {
	abandon := time.Duration(DefaultAbandonedAfterSeconds) * time.Second
	if runClaimTTL <= abandon {
		t.Fatalf("runClaimTTL (%s) must exceed the abandoned window (%s), or a live-but-slow claimant is stolen from and its run executes twice", runClaimTTL, abandon)
	}
}
