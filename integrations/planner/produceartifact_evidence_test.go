package planner

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestGeneratedOutputCountFromExecuteResult covers the three shape() output
// shapes the planner can get back from queryGeneratedOutputsForPlan:
// single-row -> bare map, multi-row -> []any, empty/absent -> 0.
func TestGeneratedOutputCountFromExecuteResult(t *testing.T) {
	row := map[string]any{"id": "v1:library:generatedOutput:x"}
	cases := []struct {
		name string
		res  any
		want int
	}{
		{"single bare-map row", map[string]any{"data": row}, 1},
		{"multi-row slice", map[string]any{"data": []any{row, row, row}}, 3},
		{"empty slice", map[string]any{"data": []any{}}, 0},
		{"no data key", map[string]any{}, 0},
		{"non-map result", "nope", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := generatedOutputCountFromExecuteResult(tc.res); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestArtifactProducedForPlan is the memql#939 core: the planner gates a
// produceArtifact plan's success on whether a v1:library:generatedOutput row
// actually landed for it. A clean-but-empty production turn (workbench write
// rejected, agent only apologised) must read as "not produced" so the plan is
// honestly marked failed instead of fake-succeeded.
func TestArtifactProducedForPlan(t *testing.T) {
	const planId = "v1:planner:plan:p1"
	const owner = "v1:identity:user:u1"
	row := map[string]any{"id": "v1:library:generatedOutput:x"}

	cases := []struct {
		name         string
		owner        string
		responder    func(query string) (any, error)
		wantProduced bool
		wantQueryOK  bool
		wantQueried  bool
	}{
		{
			name:  "a row exists -> produced",
			owner: owner,
			responder: func(q string) (any, error) {
				return map[string]any{"data": []any{row}}, nil
			},
			wantProduced: true, wantQueryOK: true, wantQueried: true,
		},
		{
			name:  "zero rows -> not produced (honest failure)",
			owner: owner,
			responder: func(q string) (any, error) {
				return map[string]any{"data": []any{}}, nil
			},
			wantProduced: false, wantQueryOK: true, wantQueried: true,
		},
		{
			name:  "query error -> inconclusive",
			owner: owner,
			responder: func(q string) (any, error) {
				return nil, fmt.Errorf("engine boom")
			},
			wantProduced: false, wantQueryOK: false, wantQueried: true,
		},
		{
			name:         "no owner -> inconclusive, no query attempted",
			owner:        "  ",
			responder:    func(q string) (any, error) { return nil, nil },
			wantProduced: false, wantQueryOK: false, wantQueried: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEngine{execResponder: tc.responder}
			p := &PlannerIntegration{logger: testLogger(), engine: fe}

			produced, queryOK := p.artifactProducedForPlan(context.Background(), planId, tc.owner)

			if produced != tc.wantProduced || queryOK != tc.wantQueryOK {
				t.Fatalf("got (produced=%v, queryOK=%v), want (%v, %v)",
					produced, queryOK, tc.wantProduced, tc.wantQueryOK)
			}

			exec, _, _ := fe.snapshot()
			queried := countContains(exec, "queryGeneratedOutputsForPlan") > 0
			if queried != tc.wantQueried {
				t.Fatalf("queried=%v, want %v (calls: %v)", queried, tc.wantQueried, exec)
			}
			if tc.wantQueried && !strings.Contains(strings.Join(exec, "|"), planId) {
				t.Fatalf("expected the lookup to carry planId %q, calls: %v", planId, exec)
			}
		})
	}
}
