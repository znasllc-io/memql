package node

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// scriptedExecutor returns a programmed error per call index, recording every
// query so #1755 upsert/retry behavior can be asserted without a DB.
type scriptedExecutor struct {
	mu      sync.Mutex
	calls   []string
	errAt   func(attempt int, query string) error
	attempt int
}

func (s *scriptedExecutor) Execute(_ context.Context, query string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt++
	s.calls = append(s.calls, query)
	if s.errAt != nil {
		return s.errAt(s.attempt, query)
	}
	return nil
}

func (s *scriptedExecutor) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

func newTestWriter(exec EngineExecutor) *NodeStatusWriter {
	return NewNodeStatusWriter(exec, nil, testLogger())
}

// #1755: a missing-row update self-heals via a createNode insert.
func TestPersist_UpsertsWhenRowMissing(t *testing.T) {
	exec := &scriptedExecutor{
		errAt: func(_ int, query string) error {
			if strings.HasPrefix(query, "updateNodeHealth") {
				return fmt.Errorf(`update(): no existing row for concept "v1:cluster:node" id "n1" (use insert() to create)`)
			}
			return nil // create succeeds
		},
	}
	w := newTestWriter(exec)
	updateQ, _ := buildUpdateNodeHealthCall("n1", "cognition", "addr:1", "healthy", "2026-06-19T00:00:00Z")
	if err := w.persistHealthTransition(context.Background(), updateQ, "n1", "cognition", "addr:1", "healthy", "2026-06-19T00:00:00Z"); err != nil {
		t.Fatalf("upsert should succeed via insert fallback, got %v", err)
	}
	calls := exec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want update then create (2 calls), got %d: %v", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "updateNodeHealth") {
		t.Errorf("first call should be the update, got %q", calls[0])
	}
	if !strings.HasPrefix(calls[1], "createNode") {
		t.Errorf("fallback should be createNode, got %q", calls[1])
	}
}

// #1755: transient errors (53300 surge) are retried; a later attempt succeeds.
func TestPersist_RetriesTransient(t *testing.T) {
	exec := &scriptedExecutor{
		errAt: func(attempt int, _ string) error {
			if attempt < 2 {
				return fmt.Errorf("read latest row ...: FATAL: sorry, too many clients already (SQLSTATE 53300)")
			}
			return nil
		},
	}
	w := newTestWriter(exec)
	updateQ, _ := buildUpdateNodeHealthCall("n2", "agent", "addr:2", "degraded", "2026-06-19T00:00:00Z")
	if err := w.persistHealthTransition(context.Background(), updateQ, "n2", "agent", "addr:2", "degraded", "2026-06-19T00:00:00Z"); err != nil {
		t.Fatalf("transient error should be retried to success, got %v", err)
	}
	if got := len(exec.snapshot()); got != 2 {
		t.Fatalf("want 2 attempts (1 transient fail + 1 success), got %d", got)
	}
}

// A non-transient, non-missing-row error is returned immediately (no retry,
// no insert fallback) so a real failure surfaces.
func TestPersist_NonTransientNoRetry(t *testing.T) {
	exec := &scriptedExecutor{
		errAt: func(_ int, _ string) error {
			return fmt.Errorf("validation: health must be one of ...")
		},
	}
	w := newTestWriter(exec)
	updateQ, _ := buildUpdateNodeHealthCall("n3", "voice", "addr:3", "healthy", "2026-06-19T00:00:00Z")
	if err := w.persistHealthTransition(context.Background(), updateQ, "n3", "voice", "addr:3", "healthy", "2026-06-19T00:00:00Z"); err == nil {
		t.Fatal("non-transient error should propagate")
	}
	if got := len(exec.snapshot()); got != 1 {
		t.Fatalf("non-transient error must not retry or fall back; want 1 call, got %d", got)
	}
}

func TestBuildCreateNodeHealthCall(t *testing.T) {
	got, err := buildCreateNodeHealthCall("n4", "planner", "addr:4", "offline", "2026-06-19T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "createNode({") {
		t.Errorf("expected createNode call, got %q", got)
	}
	for _, needle := range []string{`"id":"n4"`, `"nodeType":"planner"`, `"health":"offline"`} {
		if !strings.Contains(got, needle) {
			t.Errorf("expected %s in %q", needle, got)
		}
	}
	if _, err := buildCreateNodeHealthCall("", "planner", "a", "healthy", "t"); err == nil {
		t.Error("expected error when nodeId empty")
	}
}

func TestIsTransientAndMissingRow(t *testing.T) {
	if !isMissingRowErr(fmt.Errorf(`no existing row for concept "v1:cluster:node"`)) {
		t.Error("should detect missing-row error")
	}
	if isMissingRowErr(fmt.Errorf("some other error")) {
		t.Error("should not flag unrelated error as missing-row")
	}
	if !isTransientErr(fmt.Errorf("SQLSTATE 53300")) {
		t.Error("53300 should be transient")
	}
	if isTransientErr(fmt.Errorf("no existing row")) {
		t.Error("missing-row must NOT be treated as transient (would retry pointlessly)")
	}
	if isTransientErr(nil) {
		t.Error("nil is not transient")
	}
}

// sanity: the proto enum still round-trips (guards the constant set this fix
// depends on for HealthLabel).
func TestStatusWriterEnumPresent(t *testing.T) {
	if HealthLabel(nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY) != "healthy" {
		t.Fatal("HealthLabel drift")
	}
}

var _ = time.Second
