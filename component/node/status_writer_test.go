package node

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
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
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "updateNodeHealth(") {
		t.Errorf("expected mutation call prefix, got %q", got)
	}
	for _, needle := range []string{
		`id: "cognition-abc"`,
		`nodeType: "cognition"`,
		`address: "cognition.local:50052"`,
		`health: "degraded"`,
		`lastSeen: "2026-04-13T12:00:00Z"`,
	} {
		if !strings.Contains(got, needle) {
			t.Errorf("expected %s in mutation call, got %q", needle, got)
		}
	}
}

func TestBuildUpdateNodeHealthCall_RequiresKeyFields(t *testing.T) {
	if _, err := buildUpdateNodeHealthCall("", "cognition", "addr", "healthy", "2026-04-13T12:00:00Z", nil); err == nil {
		t.Error("expected error when nodeId is empty")
	}
	if _, err := buildUpdateNodeHealthCall("id", "", "addr", "healthy", "2026-04-13T12:00:00Z", nil); err == nil {
		t.Error("expected error when nodeType is empty")
	}
	if _, err := buildUpdateNodeHealthCall("id", "cognition", "addr", "", "2026-04-13T12:00:00Z", nil); err == nil {
		t.Error("expected error when health is empty")
	}
}

// THE MESH REPORT RENDERS AS MEMQL THAT PARSES (memql#5338, D7). The self
// heartbeat's only write channel is a call STRING, so the report -- a nested
// object carrying peer ids off the wire -- is proven here through the front
// end's own parser, not merely built. The hostile peer id carries the four
// bytes Go quoting renders as escapes the lexer refuses (NUL, BEL, VT, DEL);
// the control below proves this parser would refuse them if they leaked
// through, so a pass means the renderer escaped them.
func TestBuildUpdateNodeHealthCall_MeshReportParses(t *testing.T) {
	hostile := "pod\x00\a\v\x7f\"quote\\back"
	report := MeshReport{
		Receives: true,
		Since:    time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		Links: []MeshLink{
			{Node: "bff-5658459dd4-92gnh", Type: "bff", Via: "dialed"},
			{Node: hostile, Type: "agent", Via: "both"},
		},
		Heard: 1204, Duplicates: 310, Originated: 402, Relayed: 802,
		LastHeardAt: time.Date(2026, 9, 22, 10, 41, 2, 0, time.UTC),
	}
	call, err := buildUpdateNodeHealthCall("edge-1", "edge", "10.0.0.7:50062", "healthy",
		"2026-09-22T10:41:30Z", report.Wire())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := langparser.ParseExpression("mutation " + call); err != nil {
		t.Fatalf("the rendered heartbeat does not parse: %v\n%s", err, call)
	}
	for _, needle := range []string{`mesh: {`, `"heard":1204`, `"lastHeardAt":"2026-09-22T10:41:02Z"`, `"via":"both"`} {
		if !strings.Contains(call, needle) {
			t.Errorf("rendered call lacks %s:\n%s", needle, call)
		}
	}

	// The control: the same hostile id, Go-quoted, is refused by this parser.
	if _, err := langparser.ParseExpression("f(x: " + strconv.Quote(hostile) + ")"); err == nil {
		t.Fatal("negative control failed: Go quoting parsed, so the check above proves nothing")
	}
}

// A report with nothing heard yet renders WITHOUT lastHeardAt -- absence, not
// the epoch -- and a nil report renders no mesh argument at all, so a
// peer-transition write leaves the stored report where it was.
func TestBuildUpdateNodeHealthCall_MeshAbsences(t *testing.T) {
	fresh := MeshReport{Receives: true, Since: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)}
	call, err := buildUpdateNodeHealthCall("mcp-1", "mcp", "a:1", "healthy", "2026-09-22T10:00:30Z", fresh.Wire())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(call, "lastHeardAt") {
		t.Fatalf("a node that has heard nothing must not report a lastHeardAt:\n%s", call)
	}
	if !strings.Contains(call, `"links":[]`) {
		t.Fatalf("no links must render as an empty list, not be omitted:\n%s", call)
	}
	peer, err := buildUpdateNodeHealthCall("agent-1", "agent", "a:1", "offline", "2026-09-22T10:00:30Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(peer, "mesh") {
		t.Fatalf("a peer-transition write must carry no mesh argument:\n%s", peer)
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
			if !strings.Contains(q, `id: "agent-42"`) {
				t.Errorf("expected id in query, got %q", q)
			}
			if !strings.Contains(q, `health: "offline"`) {
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
