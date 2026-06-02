package harnesstrace

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/harness"
)

// TestParsePlanID covers the required-arg validation: planId must be a
// non-empty, non-whitespace string. The DB-bound assembly path is
// integration-covered (the assembler itself is unit-tested in
// component/harness); these are the pure paths this package adds.
func TestParsePlanID(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{"valid", map[string]any{"planId": "v1:harness:plan:abc"}, "v1:harness:plan:abc", false},
		{"trims whitespace", map[string]any{"planId": "  v1:harness:plan:xyz  "}, "v1:harness:plan:xyz", false},
		{"missing", map[string]any{}, "", true},
		{"empty string", map[string]any{"planId": ""}, "", true},
		{"whitespace only", map[string]any{"planId": "   "}, "", true},
		{"wrong type", map[string]any{"planId": 123}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePlanID(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePlanID(%v) expected error, got nil", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePlanID(%v) unexpected error: %v", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parsePlanID(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestBuildTraceNode pins the synthetic-node payload shape -- the wire
// contract the cockpit `harness trace` CLI decodes. It builds a real
// Trace via the existing assembler (no DB), packs it, and asserts the
// node envelope + JSON payload fields.
func TestBuildTraceNode(t *testing.T) {
	planID := "v1:harness:plan:demo"
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Two step transitions + one observation across one step, plus a
	// terminal plan transition, so IsComplete() is true and stepCount is 1.
	events := []harness.TraceEvent{
		{At: now, Kind: harness.EventKindPlan, NodeID: planID, Status: "running", Content: "demo goal"},
		{At: now.Add(time.Second), Kind: harness.EventKindStep, NodeID: "s1", StepID: "s1", Status: "running", Content: "step one"},
		{At: now.Add(2 * time.Second), Kind: harness.EventKindObservation, NodeID: "o1", StepID: "s1", ObservationKind: "note", Content: "did a thing"},
		{At: now.Add(3 * time.Second), Kind: harness.EventKindPlan, NodeID: planID, Status: harness.PlanStatusDone, Content: "demo goal"},
	}
	tr := harness.AssembleTrace(planID, events)

	node, err := buildTraceNode(planID, tr)
	if err != nil {
		t.Fatalf("buildTraceNode: %v", err)
	}

	// Node envelope.
	if node.ID != "harness-trace:"+planID {
		t.Fatalf("node.ID = %q, want deterministic %q", node.ID, "harness-trace:"+planID)
	}
	if node.Concept != traceConcept {
		t.Fatalf("node.Concept = %q, want %q", node.Concept, traceConcept)
	}
	if node.Type != memorynodes.NodeTypeObject {
		t.Fatalf("node.Type = %q, want %q", node.Type, memorynodes.NodeTypeObject)
	}
	if node.CreatedAt.IsZero() {
		t.Fatalf("node.CreatedAt should be stamped, got zero")
	}

	// Payload shape.
	var p tracePayload
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v\npayload=%s", err, node.Payload)
	}
	if p.PlanID != planID {
		t.Fatalf("payload.planId = %q, want %q", p.PlanID, planID)
	}
	if p.Timeline == "" {
		t.Fatalf("payload.timeline should be the rendered timeline, got empty")
	}
	if p.Timeline != tr.Render() {
		t.Fatalf("payload.timeline must equal tr.Render()")
	}
	if !p.Complete {
		t.Fatalf("payload.complete = false, want true (plan reached terminal status)")
	}
	if p.StepCount != len(tr.Steps) || p.StepCount != 1 {
		t.Fatalf("payload.stepCount = %d, want %d (len(tr.Steps))", p.StepCount, len(tr.Steps))
	}

	// Verify the exact JSON key set is the documented contract.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(node.Payload, &raw); err != nil {
		t.Fatalf("payload key unmarshal: %v", err)
	}
	for _, k := range []string{"planId", "timeline", "complete", "stepCount"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("payload missing required key %q; got keys %v", k, keysOf(raw))
		}
	}
}

// TestBuildTraceNode_EmptyTrace covers the missing-plan case: an empty
// trace still produces a well-formed node (complete=false, stepCount=0)
// rather than erroring, so the cockpit gets a clear "no such plan / not
// started" answer instead of a wire failure.
func TestBuildTraceNode_EmptyTrace(t *testing.T) {
	planID := "v1:harness:plan:missing"
	tr := harness.AssembleTrace(planID, nil)

	node, err := buildTraceNode(planID, tr)
	if err != nil {
		t.Fatalf("buildTraceNode: %v", err)
	}
	var p tracePayload
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.PlanID != planID {
		t.Fatalf("payload.planId = %q, want %q", p.PlanID, planID)
	}
	if p.Complete {
		t.Fatalf("payload.complete = true, want false for empty trace")
	}
	if p.StepCount != 0 {
		t.Fatalf("payload.stepCount = %d, want 0 for empty trace", p.StepCount)
	}
}

// TestOwnerFromContext verifies the actor-resolution hook used for
// owner-scoping mirrors recall: present auth context yields the userId,
// absent context yields empty (dev mode, fail-open read of the plan's
// own history).
func TestOwnerFromContext(t *testing.T) {
	if got := ownerFromContext(context.Background()); got != "" {
		t.Fatalf("ownerFromContext(no auth) = %q, want empty", got)
	}
	ctx := componentAuth.ContextWithAccess(context.Background(), &componentAuth.AccessContext{UserId: "v1:identity:user:alice"})
	if got := ownerFromContext(ctx); got != "v1:identity:user:alice" {
		t.Fatalf("ownerFromContext(with auth) = %q, want %q", got, "v1:identity:user:alice")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
