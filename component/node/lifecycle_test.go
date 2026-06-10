package node

import (
	"sync"
	"testing"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// TestNodeLifecycle_StartsInStarting pins the boot state.
func TestNodeLifecycle_StartsInStarting(t *testing.T) {
	lc := NewNodeLifecycle()
	if got := lc.State(); got != LifecycleStarting {
		t.Fatalf("new lifecycle state = %v, want Starting", got)
	}
	if lc.IsReady() {
		t.Fatalf("a Starting node must not report Ready")
	}
	if lc.IsDraining() {
		t.Fatalf("a Starting node must not report Draining")
	}
}

// TestNodeLifecycle_LegalForwardPath walks the full canonical progression
// Starting -> Ready -> Draining -> Stopped and asserts each edge succeeds.
func TestNodeLifecycle_LegalForwardPath(t *testing.T) {
	lc := NewNodeLifecycle()

	if err := lc.MarkReady(); err != nil {
		t.Fatalf("Starting -> Ready: unexpected error: %v", err)
	}
	if !lc.IsReady() {
		t.Fatalf("after MarkReady, IsReady() should be true")
	}

	if err := lc.MarkDraining(); err != nil {
		t.Fatalf("Ready -> Draining: unexpected error: %v", err)
	}
	if !lc.IsDraining() {
		t.Fatalf("after MarkDraining, IsDraining() should be true")
	}
	if lc.IsReady() {
		t.Fatalf("a Draining node must not report Ready")
	}

	if err := lc.MarkStopped(); err != nil {
		t.Fatalf("Draining -> Stopped: unexpected error: %v", err)
	}
	if got := lc.State(); got != LifecycleStopped {
		t.Fatalf("state = %v, want Stopped", got)
	}
	// Stopped still counts as not-ready / not-serving (draining-or-gone).
	if !lc.IsDraining() {
		t.Fatalf("a Stopped node must report IsDraining (not-serving)")
	}
}

// TestNodeLifecycle_IllegalTransitionsGuarded asserts every backward / skip
// edge that is NOT allowed returns an error and leaves the state unchanged.
func TestNodeLifecycle_IllegalTransitionsGuarded(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*NodeLifecycle)
		to    LifecycleState
	}{
		{"Draining -> Ready (no un-drain)", func(lc *NodeLifecycle) {
			_ = lc.MarkReady()
			_ = lc.MarkDraining()
		}, LifecycleReady},
		{"Draining -> Starting (no rewind)", func(lc *NodeLifecycle) {
			_ = lc.MarkReady()
			_ = lc.MarkDraining()
		}, LifecycleStarting},
		{"Ready -> Starting (no rewind)", func(lc *NodeLifecycle) {
			_ = lc.MarkReady()
		}, LifecycleStarting},
		{"Stopped -> Ready (terminal)", func(lc *NodeLifecycle) {
			_ = lc.MarkReady()
			_ = lc.MarkDraining()
			_ = lc.MarkStopped()
		}, LifecycleReady},
		{"Stopped -> Draining (terminal)", func(lc *NodeLifecycle) {
			_ = lc.MarkReady()
			_ = lc.MarkDraining()
			_ = lc.MarkStopped()
		}, LifecycleDraining},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := NewNodeLifecycle()
			tc.setup(lc)
			before := lc.State()

			if err := lc.Transition(tc.to); err == nil {
				t.Fatalf("expected illegal transition %v -> %v to error", before, tc.to)
			}
			if after := lc.State(); after != before {
				t.Fatalf("illegal transition mutated state: %v -> %v", before, after)
			}
		})
	}
}

// TestNodeLifecycle_StartingMayDrainOrStopDirectly covers the early-shutdown
// path: a node that gets SIGTERM mid-boot (before Ready) must still be able to
// drain / stop.
func TestNodeLifecycle_StartingMayDrainOrStopDirectly(t *testing.T) {
	lc := NewNodeLifecycle()
	if err := lc.MarkDraining(); err != nil {
		t.Fatalf("Starting -> Draining should be legal (early shutdown): %v", err)
	}

	lc2 := NewNodeLifecycle()
	if err := lc2.MarkStopped(); err != nil {
		t.Fatalf("Starting -> Stopped should be legal (early abort): %v", err)
	}
}

// TestNodeLifecycle_SelfTransitionIsNoOp asserts an idempotent X -> X edge
// succeeds and does NOT fire the observer.
func TestNodeLifecycle_SelfTransitionIsNoOp(t *testing.T) {
	lc := NewNodeLifecycle()
	_ = lc.MarkReady()

	var fired int
	lc.SetObserver(func(_, _ LifecycleState) { fired++ })

	if err := lc.MarkReady(); err != nil {
		t.Fatalf("idempotent Ready -> Ready should succeed: %v", err)
	}
	if fired != 0 {
		t.Fatalf("self-transition fired the observer %d times, want 0", fired)
	}
	if got := lc.State(); got != LifecycleReady {
		t.Fatalf("state after self-transition = %v, want Ready", got)
	}
}

// TestNodeLifecycle_ObserverFiresOnActualChange asserts the observer receives
// the (old, new) pair exactly once per real transition.
func TestNodeLifecycle_ObserverFiresOnActualChange(t *testing.T) {
	lc := NewNodeLifecycle()

	type change struct{ old, new LifecycleState }
	var got []change
	lc.SetObserver(func(o, n LifecycleState) { got = append(got, change{o, n}) })

	_ = lc.MarkReady()
	_ = lc.MarkDraining()

	want := []change{
		{LifecycleStarting, LifecycleReady},
		{LifecycleReady, LifecycleDraining},
	}
	if len(got) != len(want) {
		t.Fatalf("observer fired %d times, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestNodeLifecycle_HealthMapping pins the lifecycle -> gossip NodeHealthStatus
// mapping that peers route by.
func TestNodeLifecycle_HealthMapping(t *testing.T) {
	cases := []struct {
		state LifecycleState
		want  nodev1.NodeHealthStatus
	}{
		{LifecycleStarting, nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING},
		{LifecycleReady, nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
		{LifecycleDraining, nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING},
		{LifecycleStopped, nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED},
	}
	for _, tc := range cases {
		if got := tc.state.Health(); got != tc.want {
			t.Fatalf("%v.Health() = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// TestNodeLifecycle_HealthFlipsOnDraining is the gossip-advertisement
// assertion: a node's advertised health goes HEALTHY -> DRAINING when its
// lifecycle flips to Draining, which is what later phases use to route away
// from a draining node.
func TestNodeLifecycle_HealthFlipsOnDraining(t *testing.T) {
	lc := NewNodeLifecycle()
	_ = lc.MarkReady()
	if got := lc.Health(); got != nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
		t.Fatalf("Ready node advertises %v, want HEALTHY", got)
	}

	_ = lc.MarkDraining()
	if got := lc.Health(); got != nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING {
		t.Fatalf("Draining node advertises %v, want DRAINING", got)
	}
}

// TestNodeLifecycle_ConcurrentTransitions is a race-detector smoke test: the
// node is multi-goroutine, so concurrent readers + a writer must not race and
// the terminal state must be deterministic.
func TestNodeLifecycle_ConcurrentTransitions(t *testing.T) {
	lc := NewNodeLifecycle()
	_ = lc.MarkReady()

	var wg sync.WaitGroup
	// Many concurrent readers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lc.State()
			_ = lc.IsReady()
			_ = lc.IsDraining()
			_ = lc.Health()
		}()
	}
	// Concurrent writers all racing to Draining -- exactly one wins the
	// transition, the rest are idempotent no-ops; none error.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lc.MarkDraining(); err != nil {
				t.Errorf("concurrent MarkDraining errored: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := lc.State(); got != LifecycleDraining {
		t.Fatalf("terminal state = %v, want Draining", got)
	}
}

// TestPeerConnection_HeartbeatAdvertisesLifecycleHealth asserts the outbound
// heartbeat builder stamps the lifecycle health from the wired healthFn, and
// falls back to HEALTHY when none is wired (backward-compatible gossip wire).
func TestPeerConnection_HeartbeatAdvertisesLifecycleHealth(t *testing.T) {
	pm := NewPeerManager(&Identity{ID: "n1", Type: NodeTypeBFF}, testLogger())
	pc := newPeerConnection(testIdentity(), "peer", "addr:1", testLogger())

	// No healthFn wired -> HEALTHY default (pre-lifecycle behaviour).
	if got := selfHeartbeatHealth(pc); got != nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
		t.Fatalf("default heartbeat health = %v, want HEALTHY", got)
	}

	// Wire the lifecycle source the way ParentConnector/WorkerDialer do.
	pc.SetHealthFn(pm.Lifecycle().Health)

	_ = pm.Lifecycle().MarkReady()
	if got := selfHeartbeatHealth(pc); got != nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
		t.Fatalf("Ready heartbeat health = %v, want HEALTHY", got)
	}

	_ = pm.Lifecycle().MarkDraining()
	if got := selfHeartbeatHealth(pc); got != nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING {
		t.Fatalf("Draining heartbeat health = %v, want DRAINING", got)
	}
}

// selfHeartbeatHealth resolves what sendHeartbeatMessage would stamp, without
// needing a live stream. Mirrors the resolution logic in
// peerConnection.sendHeartbeatMessage.
func selfHeartbeatHealth(pc *peerConnection) nodev1.NodeHealthStatus {
	pc.mu.Lock()
	fn := pc.healthFn
	pc.mu.Unlock()
	health := nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
	if fn != nil {
		if h := fn(); h != nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
			health = h
		}
	}
	return health
}

// TestBuildServerHeartbeat_CarriesLifecycleHealth pins the server-direction
// heartbeat builder: it stamps the supplied lifecycle health and defaults an
// UNSPECIFIED to HEALTHY.
func TestBuildServerHeartbeat_CarriesLifecycleHealth(t *testing.T) {
	hb := buildServerHeartbeat(nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING)
	if got := hb.GetHeartbeat().GetHealth(); got != nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING {
		t.Fatalf("server heartbeat health = %v, want DRAINING", got)
	}

	def := buildServerHeartbeat(nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED)
	if got := def.GetHeartbeat().GetHealth(); got != nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
		t.Fatalf("UNSPECIFIED server heartbeat health = %v, want HEALTHY default", got)
	}
}

// TestPeerManager_ExposesLifecycle asserts the PeerManager owns a non-nil
// lifecycle that starts in Starting -- the wiring the heartbeat builders and
// the readiness bridge depend on.
func TestPeerManager_ExposesLifecycle(t *testing.T) {
	pm := NewPeerManager(&Identity{ID: "n1", Type: NodeTypeBFF}, testLogger())
	lc := pm.Lifecycle()
	if lc == nil {
		t.Fatalf("PeerManager.Lifecycle() returned nil")
	}
	if got := lc.State(); got != LifecycleStarting {
		t.Fatalf("PeerManager lifecycle starts at %v, want Starting", got)
	}
	if got := lc.Health(); got != nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING {
		t.Fatalf("Starting node advertises %v, want CONNECTING", got)
	}
}
