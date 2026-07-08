package dailyspace

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestExtractRows_NilExecuteResult guards the nil-pointer branch
// on the *memql.ExecuteResult unwrap. The bigger payload-projection
// + bundle paths are covered indirectly via the live debug-trace
// loop documented in memql-cockpit#49 (userById was returning
// 0 rows because the extractor had no case for the *ExecuteResult
// Go type the engine returns; the fix adds the unwrap branch).
func TestExtractRows_NilExecuteResult(t *testing.T) {
	var res *memql.ExecuteResult
	if got := extractRows(res); len(got) != 0 {
		t.Errorf("expected 0 rows from nil ExecuteResult, got %d", len(got))
	}
}

// TestExtractRows_LegacyShapesStillWork pins backwards-compat for
// the value-level type switch that used to be the entire function.
// Callers that pre-flatten to map / slice shapes (cognition's
// query path, fixture-driven tests) keep working.
func TestExtractRows_LegacyShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 0},
		{"map-single", map[string]any{"id": "abc", "email": "test@example.com"}, 1},
		{"slice-of-maps", []map[string]any{{"id": "a"}, {"id": "b"}}, 2},
		{"slice-of-any", []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}, 2},
		{"unknown-type", 42, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRows(tc.in)
			if len(got) != tc.want {
				t.Errorf("expected %d rows, got %d", tc.want, len(got))
			}
		})
	}
}

// TestHandleEnsureForUser_EmptyUserIdNoOps (memql#1065) pins that a
// truly-empty resolution (no userId AND no subject reached the builtin)
// no-ops with a skip node instead of erroring. The login automation
// must not abort its run when a genuinely user-less session lands; the
// happy path passes the JWT subject when userId is absent, so this
// branch only fires when BOTH are empty.
func TestHandleEnsureForUser_EmptyUserIdNoOps(t *testing.T) {
	d := &Integration{}
	for _, userId := range []any{"", "   ", nil} {
		nodes, err := d.handleEnsureForUser(nil, map[string]any{"userId": userId}, 0)
		if err != nil {
			t.Fatalf("userId=%#v: expected no-op, got error: %v", userId, err)
		}
		if len(nodes) != 1 {
			t.Fatalf("userId=%#v: expected one skip node, got %d", userId, len(nodes))
		}
	}
}
