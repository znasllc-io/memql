package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// stubExecutor records every MemQL query passed to Execute so tests can
// assert the NodeStatusWriter produces well-formed mutation calls.
type stubExecutor struct {
	mu      sync.Mutex
	queries []string
}

func (s *stubExecutor) Execute(_ context.Context, query string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, query)
	return nil
}

func (s *stubExecutor) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.queries))
	copy(out, s.queries)
	return out
}

func TestBuildUpdateNodeHealthCall_MinimalFields(t *testing.T) {
	got, err := buildUpdateNodeHealthCall(
		"cognition-abc",
		"cognition",
		"cognition.local:50052",
		"degraded",
		"2026-04-13T12:00:00Z",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "updateNodeHealth({") {
		t.Errorf("expected mutation call prefix, got %q", got)
	}
	for _, needle := range []string{
		`"id":"cognition-abc"`,
		`"nodeType":"cognition"`,
		`"address":"cognition.local:50052"`,
		`"health":"degraded"`,
		`"lastSeen":"2026-04-13T12:00:00Z"`,
	} {
		if !strings.Contains(got, needle) {
			t.Errorf("expected %s in mutation call, got %q", needle, got)
		}
	}
}

func TestBuildUpdateNodeHealthCall_RequiresKeyFields(t *testing.T) {
	if _, err := buildUpdateNodeHealthCall("", "cognition", "addr", "healthy", "2026-04-13T12:00:00Z"); err == nil {
		t.Error("expected error when nodeId is empty")
	}
	if _, err := buildUpdateNodeHealthCall("id", "", "addr", "healthy", "2026-04-13T12:00:00Z"); err == nil {
		t.Error("expected error when nodeType is empty")
	}
	if _, err := buildUpdateNodeHealthCall("id", "cognition", "addr", "", "2026-04-13T12:00:00Z"); err == nil {
		t.Error("expected error when health is empty")
	}
}

func TestHealthLabelRoundTrip(t *testing.T) {
	cases := []nodev1.NodeHealthStatus{
		nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING,
		nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
		nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING,
		nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
		nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED,
	}
	for _, c := range cases {
		got := ParseHealthLabel(HealthLabel(c))
		if got != c {
			t.Errorf("round-trip mismatch: %v -> %q -> %v", c, HealthLabel(c), got)
		}
	}
}

// TestNodeStatusWriter_EmitsMutationOnHandle verifies that invoking the
// writer's Handle eventually submits an updateNodeHealth call to the
// executor. The handler is async, so we poll briefly.
func TestNodeStatusWriter_EmitsMutationOnHandle(t *testing.T) {
	exec := &stubExecutor{}
	w := NewNodeStatusWriter(exec, testIdentity(), testLogger())

	w.Handle(
		context.Background(),
		&nodev1.PeerInfo{
			NodeId:   "agent-42",
			NodeType: "agent",
			Address:  "a42:50052",
			Health:   nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
		},
		nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
		nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
		time.Now(),
	)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if qs := exec.snapshot(); len(qs) > 0 {
			q := qs[0]
			if !strings.Contains(q, `"id":"agent-42"`) {
				t.Errorf("expected id in query, got %q", q)
			}
			if !strings.Contains(q, `"health":"offline"`) {
				t.Errorf("expected health=offline, got %q", q)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("writer did not submit a query within 500ms")
}

// TestNodeStatusWriter_NilEngineIsNoop protects against test fixtures
// that install a writer without an engine (a real bootstrap scenario when
// the engine hasn't finished initializing yet).
func TestNodeStatusWriter_NilEngineIsNoop(t *testing.T) {
	w := NewNodeStatusWriter(nil, testIdentity(), testLogger())
	// Should not panic, should not block, should not go routine-leak.
	w.Handle(
		context.Background(),
		&nodev1.PeerInfo{NodeId: "x", NodeType: "agent"},
		nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
		time.Now(),
	)
}
