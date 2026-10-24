package consent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock yields the value the test most recently set. Concurrent-
// safe so the broadcast/subscribe paths can race against grant
// transitions without flaking.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock    { return &fakeClock{now: t} }
func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestManager_AllowsRejectsWithoutGrant(t *testing.T) {
	m := NewManager()
	dec := m.Allows("workerComputer", "mouse_click")
	if dec.Allowed {
		t.Fatal("first call must be rejected -- no consent granted yet")
	}
	if !strings.Contains(dec.Reason, "consent has not been granted") {
		t.Errorf("rejection reason should hint at the grant flow; got %q", dec.Reason)
	}
}

func TestManager_GrantedWindowAdmits(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	for _, tc := range []struct {
		tool, action string
	}{
		{"workerHost", "exec"},
		{"workerHost", "fs_read"},
		{"workerComputer", "mouse_click"},
		{"workerComputer", "screenshot"},
	} {
		dec := m.Allows(tc.tool, tc.action)
		if !dec.Allowed {
			t.Errorf("%s.%s should be admitted within an open window; reason=%s", tc.tool, tc.action, dec.Reason)
		}
	}
}

func TestManager_RevokeImmediatelyDenies(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if dec := m.Allows("workerHost", "exec"); !dec.Allowed {
		t.Fatalf("expected admit pre-revoke; got %v", dec)
	}
	m.Revoke()
	dec := m.Allows("workerHost", "exec")
	if dec.Allowed {
		t.Errorf("post-revoke call must be denied")
	}
	if !strings.Contains(dec.Reason, "consent has not been granted") {
		t.Errorf("post-revoke reason should match the no-window reason; got %q", dec.Reason)
	}
}

func TestManager_WindowExpiryDenies(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	clk := newFakeClock(start)
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(10*time.Minute, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Mid-window: admit.
	clk.advance(5 * time.Minute)
	if dec := m.Allows("workerHost", "exec"); !dec.Allowed {
		t.Fatalf("mid-window call should admit; got %v", dec)
	}
	// Past expiry: deny.
	clk.advance(6 * time.Minute) // total = 11m, window was 10m
	dec := m.Allows("workerHost", "exec")
	if dec.Allowed {
		t.Errorf("post-expiry call must be denied")
	}
	if !strings.Contains(dec.Reason, "expired") {
		t.Errorf("post-expiry reason should mention expiry; got %q", dec.Reason)
	}
}

func TestManager_SecondGrantOverwrites(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant 1: %v", err)
	}
	first := m.Snapshot().ExpiresAt
	clk.advance(5 * time.Minute)
	if _, err := m.Grant(2*time.Hour, true, nil); err != nil {
		t.Fatalf("Grant 2: %v", err)
	}
	second := m.Snapshot().ExpiresAt
	if !second.After(first) {
		t.Errorf("second Grant should advance expiry; got first=%v second=%v", first, second)
	}
	if !m.Snapshot().Strict {
		t.Errorf("second Grant set strict=true; snapshot should reflect it")
	}
}

func TestManager_NegativeWindowRejected(t *testing.T) {
	m := NewManager()
	if _, err := m.Grant(0, false, nil); err == nil {
		t.Error("Grant(0) should reject")
	}
	if _, err := m.Grant(-time.Second, false, nil); err == nil {
		t.Error("Grant(-1s) should reject")
	}
}

func TestManager_SubscribeReceivesEvents(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)
	ch, cancel := m.Subscribe()
	defer cancel()

	if _, err := m.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	m.Allows("workerHost", "exec")
	m.Revoke()

	// Drain the channel and inspect what arrived. We expect 3
	// events (granted, dispatch, revoked) in order.
	type seen struct{ kind EventKind }
	var got []seen
	timeout := time.After(time.Second)
	for len(got) < 3 {
		select {
		case ev := <-ch:
			got = append(got, seen{kind: ev.Kind})
		case <-timeout:
			t.Fatalf("only %d events arrived: %v", len(got), got)
		}
	}
	want := []seen{{EventGranted}, {EventDispatch}, {EventRevoked}}
	for i := range want {
		if got[i].kind != want[i].kind {
			t.Errorf("event[%d] = %q, want %q", i, got[i].kind, want[i].kind)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		tool, action string
		want         Class
	}{
		{"workerHost", "fs_read", ClassObserve},
		{"workerHost", "fs_list", ClassObserve},
		{"workerHost", "fs_stat", ClassObserve},
		{"workerHost", "http_fetch", ClassObserve},
		{"workerHost", "exec", ClassInteract},
		{"workerHost", "fs_write", ClassInteract},
		// A CI step runs a repository's own commands here (memql#5494):
		// interact, whoever admits it.
		{"workerHost", "pipeline_step", ClassInteract},
		// The full workerComputer action vocabulary, pinned
		// explicitly so a new action can't silently ride the
		// unknown->interact default (memql-cockpit#179).
		{"workerComputer", "screenshot", ClassObserve},
		{"workerComputer", "cursor_position", ClassObserve},
		{"workerComputer", "display_info", ClassObserve},
		{"workerComputer", "capabilities", ClassObserve},
		{"workerComputer", "window_list", ClassObserve},
		{"workerComputer", "wait", ClassObserve},
		{"workerComputer", "mouse_click", ClassInteract},
		{"workerComputer", "mouse_down", ClassInteract},
		{"workerComputer", "mouse_up", ClassInteract},
		{"workerComputer", "mouse_move", ClassInteract},
		{"workerComputer", "mouse_drag", ClassInteract},
		{"workerComputer", "mouse_scroll", ClassInteract},
		{"workerComputer", "key_type", ClassInteract},
		{"workerComputer", "key_press", ClassInteract},
		{"workerComputer", "key_hold", ClassInteract},
		{"workerComputer", "key_combo", ClassInteract},
		{"workerComputer", "window_focus", ClassInteract},
		{"workerHost", "experimental_new_action", ClassUnknown},
		{"workerOther", "foo", ClassUnknown},
		{"  WORKERHOST  ", "  FS_READ  ", ClassObserve}, // case + whitespace
	}
	for _, tc := range cases {
		got := Classify(tc.tool, tc.action)
		if got != tc.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", tc.tool, tc.action, got, tc.want)
		}
	}
}

// A pipeline step is admitted by the owner's standing consent in
// policy.yaml, not by a consent window (memql#5494) -- and it is the only
// pair that is. Everything else, the unknown included, still asks the
// window.
func TestOnlyAPipelineStepIsAdmittedByPolicyRatherThanAWindow(t *testing.T) {
	for _, pair := range [][2]string{
		{"workerHost", "pipeline_step"},
		{"  WORKERHOST  ", "  PIPELINE_STEP  "},
	} {
		if !AdmittedByPolicy(pair[0], pair[1]) {
			t.Errorf("AdmittedByPolicy(%q, %q) = false, want true", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{"workerHost", "exec"},
		{"workerHost", "fs_write"},
		{"workerHost", "fs_read"},
		{"workerHost", "experimental_new_action"},
		{"workerComputer", "pipeline_step"},
		{"workerComputer", "mouse_click"},
	} {
		if AdmittedByPolicy(pair[0], pair[1]) {
			t.Errorf("AdmittedByPolicy(%q, %q) = true: that call would skip the consent window", pair[0], pair[1])
		}
	}
}

func TestSnapshot_GrantedShowsExpiry(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if snap := m.Snapshot(); snap.Granted {
		t.Fatal("Snapshot before any Grant should report Granted=false")
	}
	if _, err := m.Grant(30*time.Minute, true, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	snap := m.Snapshot()
	if !snap.Granted {
		t.Error("Snapshot after Grant should report Granted=true")
	}
	if snap.Window != 30*time.Minute {
		t.Errorf("Snapshot.Window = %v, want 30m", snap.Window)
	}
	if !snap.Strict {
		t.Error("Snapshot.Strict should reflect the Grant's strict flag")
	}
	// Advance past expiry; Snapshot should flip back to ungranted.
	clk.advance(31 * time.Minute)
	if snap := m.Snapshot(); snap.Granted {
		t.Error("Snapshot after expiry should report Granted=false")
	}
}

// --- 64-B: strict-mode per-action approval ---

// TestStrictMode_KeyTypeBlocksAwaitingApproval drives the load-bearing
// path: an open strict window admits ClassObserve immediately, but
// workerComputer.key_type now blocks on a pending approval until an
// operator clicks Allow.
func TestStrictMode_KeyTypeBlocksAwaitingApproval(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(2 * time.Second)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	// ClassObserve under a strict window still admits without a gate.
	if dec := m.Allows("workerComputer", "screenshot"); !dec.Allowed {
		t.Errorf("screenshot must still admit under strict: %s", dec.Reason)
	}

	// key_type blocks; another goroutine watches the pending queue
	// and Approves it.
	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "key_type")
	}()

	// Poll for the pending entry to land (registration is racy with
	// the goroutine schedule; 100ms is generous enough not to flake).
	deadline := time.Now().Add(500 * time.Millisecond)
	var pending []PendingApprovalInfo
	for time.Now().Before(deadline) {
		pending = m.PendingApprovals()
		if len(pending) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending approval, got %d", len(pending))
	}
	if pending[0].Tool != "workerComputer" || pending[0].Action != "key_type" {
		t.Errorf("pending entry mis-shaped: %+v", pending[0])
	}

	// Approve and verify the blocked call returns Allowed=true.
	if err := m.Approve(pending[0].Id); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	select {
	case dec := <-done:
		if !dec.Allowed {
			t.Errorf("key_type after Approve must admit; reason=%s", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Approve")
	}
	// Pending queue drained.
	if got := m.PendingApprovals(); len(got) != 0 {
		t.Errorf("pending queue should be empty after Approve; got %d", len(got))
	}
}

// TestStrictMode_MouseClickDeny exercises the deny path -- a denied
// strict-mode call returns Allowed=false with the deny reason in
// the Decision.
func TestStrictMode_MouseClickDeny(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(2 * time.Second)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "mouse_click")
	}()
	pending := waitForPending(t, m, 1)
	if err := m.Deny(pending[0].Id); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	select {
	case dec := <-done:
		if dec.Allowed {
			t.Error("mouse_click after Deny must reject")
		}
		if !strings.Contains(dec.Reason, "denied per-action") {
			t.Errorf("reject reason should mention per-action denial; got %q", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Deny")
	}
}

// TestStrictMode_TimeoutDenies pins the timeout safety: an
// unanswered approval defaults to deny once the timeout fires.
func TestStrictMode_TimeoutDenies(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	dec := m.Allows("workerComputer", "key_type")
	if dec.Allowed {
		t.Error("timeout should default to deny")
	}
	if !strings.Contains(dec.Reason, "timed out") {
		t.Errorf("deny reason should mention timeout; got %q", dec.Reason)
	}
	if got := m.PendingApprovals(); len(got) != 0 {
		t.Errorf("pending queue should be empty after timeout; got %d", len(got))
	}
}

// TestStrictMode_RevokeCancelsPending pins the kill-switch
// behaviour: Revoke must drop every in-flight strict-mode
// approval. The blocked dispatcher goroutine receives an
// immediate deny carrying the revoke reason.
func TestStrictMode_RevokeCancelsPending(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(5 * time.Second)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "key_type")
	}()
	waitForPending(t, m, 1)

	m.Revoke()

	select {
	case dec := <-done:
		if dec.Allowed {
			t.Error("revoke during pending approval must deny")
		}
		if !strings.Contains(dec.Reason, "consent revoked") {
			t.Errorf("deny reason should mention revoke; got %q", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Revoke")
	}
}

// TestStrictMode_HighRiskSubset confirms the gate ONLY applies to
// the key_type + mouse_click subset. The other ClassInteract calls
// stay admitted by the standing window.
func TestStrictMode_HighRiskSubset(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	for _, tc := range []struct {
		tool, action string
		wantBlocked  bool
	}{
		{"workerComputer", "key_type", true},
		{"workerComputer", "mouse_click", true},
		{"workerComputer", "key_hold", true},   // composes into chords (memql-cockpit#166)
		{"workerComputer", "mouse_down", true}, // down/up pair IS a click
		{"workerComputer", "mouse_up", true},
		{"workerComputer", "key_press", false}, // ClassInteract but NOT high-risk
		{"workerComputer", "mouse_move", false},
		{"workerHost", "exec", false}, // ClassInteract but workerHost, not workerComputer
		{"workerHost", "fs_write", false},
		{"workerComputer", "screenshot", false}, // ClassObserve
		{"workerComputer", "wait", false},       // ClassObserve (memql-cockpit#166)
	} {
		t.Run(tc.tool+"."+tc.action, func(t *testing.T) {
			dec := m.Allows(tc.tool, tc.action)
			if tc.wantBlocked {
				// blocked subset: with no operator response, the
				// 40ms timeout fires and the call denies.
				if dec.Allowed {
					t.Errorf("high-risk %s.%s must be gated by strict mode", tc.tool, tc.action)
				}
			} else {
				if !dec.Allowed {
					t.Errorf("non-high-risk %s.%s must admit under strict mode; reason=%s",
						tc.tool, tc.action, dec.Reason)
				}
			}
		})
	}
}

// TestStrictMode_NonStrictBypassesApproval pins that a NON-strict
// open window still admits key_type / mouse_click without going
// through the approval gate.
func TestStrictMode_NonStrictBypassesApproval(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant non-strict: %v", err)
	}
	dec := m.Allows("workerComputer", "key_type")
	if !dec.Allowed {
		t.Errorf("non-strict window must admit key_type without approval; reason=%s", dec.Reason)
	}
}

// TestStrictMode_BroadcastsApprovalRequestedEvent confirms the
// EventApprovalRequested broadcast lands on subscribers with the
// id + tool + action populated -- this is the event the TUI's
// modal queue is driven by.
func TestStrictMode_BroadcastsApprovalRequestedEvent(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	// Subscribe BEFORE Grant so the EventGranted broadcast lands
	// in the channel and we don't race the Subscribe registration.
	ch, cancel := m.Subscribe()
	defer cancel()
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	// Drain the EventGranted broadcast.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected EventGranted before approval flow")
	}

	go m.Allows("workerComputer", "key_type")

	var requested Event
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			if ev.Kind == EventApprovalRequested {
				requested = ev
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
		if requested.Kind != "" {
			break
		}
	}
	if requested.Kind != EventApprovalRequested {
		t.Fatalf("expected EventApprovalRequested broadcast, got kind=%q", requested.Kind)
	}
	if requested.ApprovalId == "" {
		t.Error("EventApprovalRequested must carry an approval_id")
	}
	if requested.Tool != "workerComputer" || requested.Action != "key_type" {
		t.Errorf("EventApprovalRequested fields wrong: %+v", requested)
	}
}

// TestApprove_UnknownIdErrors confirms a stale Allow (after timeout
// or revoke) reports an error to the TUI so its modal can clear
// gracefully.
func TestApprove_UnknownIdErrors(t *testing.T) {
	m := NewManager()
	if err := m.Approve("not-a-real-id"); err == nil {
		t.Error("Approve must error on unknown id")
	}
	if err := m.Deny("not-a-real-id"); err == nil {
		t.Error("Deny must error on unknown id")
	}
}

// TestIsHighRiskAction guards the high-risk classifier so accidental
// classification changes (e.g. dropping mouse_click) fail loudly.
func TestIsHighRiskAction(t *testing.T) {
	for _, tc := range []struct {
		tool, action string
		want         bool
	}{
		{"workerComputer", "key_type", true},
		{"workerComputer", "mouse_click", true},
		{"workerComputer", "key_hold", true},
		{"workerComputer", "mouse_down", true},
		{"workerComputer", "mouse_up", true},
		{"workerComputer", "KEY_TYPE", true}, // case + whitespace tolerant
		{" workerComputer ", " mouse_click ", true},
		{" workerComputer ", " MOUSE_DOWN ", true},
		{"workerComputer", "mouse_move", false},
		{"workerComputer", "key_press", false},
		{"workerComputer", "screenshot", false},
		{"workerComputer", "wait", false},
		{"workerHost", "exec", false},
		{"workerHost", "fs_write", false},
		{"", "", false},
	} {
		if got := isHighRiskAction(tc.tool, tc.action); got != tc.want {
			t.Errorf("isHighRiskAction(%q, %q) = %v, want %v", tc.tool, tc.action, got, tc.want)
		}
	}
}

// waitForPending polls until at least `n` approvals are pending or
// the deadline fires. Helper used by the strict-mode tests to avoid
// duplicating the race-safe poll loop.
func waitForPending(t *testing.T, m *Manager, n int) []PendingApprovalInfo {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pending := m.PendingApprovals()
		if len(pending) >= n {
			return pending
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending approvals", n)
	return nil
}

// --- 131: strict-mode region exemption ---

// TestRegion_Contains pins the half-open rect containment used by
// the in-region exemption.
func TestRegion_Contains(t *testing.T) {
	r := Region{X: 100, Y: 100, W: 200, H: 150}
	cases := []struct {
		px, py int
		want   bool
	}{
		{100, 100, true},  // top-left corner inside
		{299, 249, true},  // last interior pixel
		{300, 249, false}, // right edge exclusive
		{299, 250, false}, // bottom edge exclusive
		{200, 175, true},  // interior
		{99, 175, false},  // left of region
		{200, 99, false},  // above region
		{0, 0, false},
	}
	for _, c := range cases {
		if got := r.Contains(c.px, c.py); got != c.want {
			t.Errorf("Region%+v.Contains(%d,%d) = %v, want %v", r, c.px, c.py, got, c.want)
		}
	}
	// Zero-area regions contain nothing.
	if (Region{X: 10, Y: 10, W: 0, H: 50}).Contains(10, 10) {
		t.Error("zero-width region must contain nothing")
	}
	if (Region{X: 10, Y: 10, W: 50, H: 0}).Contains(10, 10) {
		t.Error("zero-height region must contain nothing")
	}
}

// TestRegion_InRegionClickSkipsApproval: a strict grant carrying a
// region admits a mouse_click whose cursor is inside the rect
// WITHOUT going through the per-action approval gate.
func TestRegion_InRegionClickSkipsApproval(t *testing.T) {
	m := NewManager()
	// Tiny timeout: if the call wrongly hit the approval gate the
	// test would still finish fast (deny), and the assertion below
	// catches the wrong outcome.
	m.SetApprovalTimeout(40 * time.Millisecond)
	region := &Region{X: 100, Y: 100, W: 400, H: 300}
	if _, err := m.Grant(time.Hour, true, region); err != nil {
		t.Fatalf("Grant strict+region: %v", err)
	}
	// Cursor inside the region.
	dec := m.AllowsAt("workerComputer", "mouse_click", CursorPoint{X: 250, Y: 200, Known: true})
	if !dec.Allowed {
		t.Errorf("in-region click must admit without approval; reason=%s", dec.Reason)
	}
	if !strings.Contains(dec.Reason, "in-region") {
		t.Errorf("admit reason should name the in-region exemption; got %q", dec.Reason)
	}
}

// TestRegion_OutOfRegionClickGated: a click outside the region
// still routes through the approval gate (here: times out -> deny).
func TestRegion_OutOfRegionClickGated(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	region := &Region{X: 100, Y: 100, W: 400, H: 300}
	if _, err := m.Grant(time.Hour, true, region); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Cursor far outside the region.
	dec := m.AllowsAt("workerComputer", "mouse_click", CursorPoint{X: 1000, Y: 900, Known: true})
	if dec.Allowed {
		t.Error("out-of-region click must be gated (timed out -> deny here)")
	}
	if !strings.Contains(dec.Reason, "timed out") {
		t.Errorf("out-of-region click should have hit the approval gate; got %q", dec.Reason)
	}
}

// TestRegion_UnknownCursorGated: an unknown cursor (headless build,
// or a non-mouse_click path) can never be in-region -- it falls
// through to the gate even with a region set.
func TestRegion_UnknownCursorGated(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	region := &Region{X: 0, Y: 0, W: 9999, H: 9999} // covers everything
	if _, err := m.Grant(time.Hour, true, region); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	dec := m.AllowsAt("workerComputer", "mouse_click", CursorPoint{Known: false})
	if dec.Allowed {
		t.Error("unknown cursor must be gated even with an all-covering region")
	}
}

// TestRegion_KeyTypeAlwaysGated: key_type carries no cursor, so the
// region exemption never applies to it -- it stays gated under
// strict mode even when a region is set and would otherwise cover
// the whole screen.
func TestRegion_KeyTypeAlwaysGated(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	region := &Region{X: 0, Y: 0, W: 9999, H: 9999}
	if _, err := m.Grant(time.Hour, true, region); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Even with a cursor that's "in region", key_type ignores it.
	dec := m.AllowsAt("workerComputer", "key_type", CursorPoint{X: 10, Y: 10, Known: true})
	if dec.Allowed {
		t.Error("key_type must stay gated under strict mode regardless of region")
	}
}

// TestRegion_NonStrictIgnoresRegion: a non-strict grant drops any
// region passed to it -- there's no gate to exempt from.
func TestRegion_NonStrictIgnoresRegion(t *testing.T) {
	m := NewManager()
	region := &Region{X: 0, Y: 0, W: 100, H: 100}
	if _, err := m.Grant(time.Hour, false, region); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if snap := m.Snapshot(); snap.Region != nil {
		t.Errorf("non-strict grant must not carry a region; got %+v", snap.Region)
	}
	// A click anywhere still admits (non-strict window).
	dec := m.AllowsAt("workerComputer", "mouse_click", CursorPoint{X: 5000, Y: 5000, Known: true})
	if !dec.Allowed {
		t.Errorf("non-strict window must admit any click; reason=%s", dec.Reason)
	}
}

// TestRegion_StrictNoRegionGatesAll: a strict grant with NO region
// gates every high-risk call -- the 64-B behaviour, unchanged.
func TestRegion_StrictNoRegionGatesAll(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	dec := m.AllowsAt("workerComputer", "mouse_click", CursorPoint{X: 50, Y: 50, Known: true})
	if dec.Allowed {
		t.Error("strict + no region must gate every mouse_click")
	}
}

// TestRegion_SnapshotAndEventCarryRegion confirms the region rides
// the Status snapshot + the EventGranted broadcast so the TUI can
// render it.
func TestRegion_SnapshotAndEventCarryRegion(t *testing.T) {
	m := NewManager()
	ch, cancel := m.Subscribe()
	defer cancel()
	region := &Region{X: 12, Y: 34, W: 56, H: 78}
	if _, err := m.Grant(time.Hour, true, region); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	snap := m.Snapshot()
	if snap.Region == nil || *snap.Region != *region {
		t.Errorf("Snapshot.Region = %+v, want %+v", snap.Region, region)
	}
	select {
	case ev := <-ch:
		if ev.Kind != EventGranted {
			t.Fatalf("expected EventGranted, got %q", ev.Kind)
		}
		if ev.Region == nil || *ev.Region != *region {
			t.Errorf("EventGranted.Region = %+v, want %+v", ev.Region, region)
		}
	case <-time.After(time.Second):
		t.Fatal("no EventGranted broadcast")
	}
}

// TestIsMouseClick guards the click classifier the region exemption
// keys off.
func TestIsMouseClick(t *testing.T) {
	for _, c := range []struct {
		tool, action string
		want         bool
	}{
		{"workerComputer", "mouse_click", true},
		{"workerComputer", "MOUSE_CLICK", true},
		{" workerComputer ", " mouse_click ", true},
		{"workerComputer", "key_type", false},
		{"workerComputer", "mouse_move", false},
		{"workerHost", "exec", false},
	} {
		if got := isMouseClick(c.tool, c.action); got != c.want {
			t.Errorf("isMouseClick(%q,%q) = %v, want %v", c.tool, c.action, got, c.want)
		}
	}
}
