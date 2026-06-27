package conformance

// dim_engine_test.go -- the DB-free engine-contract dimensions:
//   #1712 system-generated IDs pass the engine ID validator,
//   #1707 entry-point logics are invokable via a wrapping automation,
//   #1705 LiteralValueNode / bare-literal AST evaluation.

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// --- #1712: system ID generators ---

func systemIDCheck() check {
	return check{
		Issue:   "#1712",
		Dim:     "system-generated-ids-valid",
		NeedsDB: false,
		Run:     runSystemID,
	}
}

func runSystemID(t *testing.T, e *Env) {
	// Colon-laden, realistic inputs drawn from the live #1712 failure: a system
	// generator must emit a colon-free shortId that passes the validator for ANY
	// concept, no matter how compound the source context is.
	inputs := []string{
		"v1:planner:responsibility:d6ac657f-1234-5678-9abc-def012345678",
		"v1:knowledge:knowledgeDomain:sales-playbook",
		"v1:planner:plan:abc-123",
		"bare-already",
		"",
	}
	concepts := []string{"v1:planner:plan", "v1:agents:agent", ""}

	assert := func(got string) {
		if strings.ContainsRune(got, ':') {
			t.Errorf("generated id %q contains a colon (would be rejected as a shortId)", got)
		}
		for _, c := range concepts {
			if err := id.ValidateShortId(c, got); err != nil {
				t.Errorf("generated id %q rejected for concept %q: %v", got, c, err)
			}
		}
	}

	n := 0
	for _, in := range inputs {
		assert(id.NewSystemNodeShortId("proactive", in))
		assert(id.NewSystemNodeShortId("refresh", in))
		assert(id.SystemNodeShortId("train", in))
		n += 3
	}

	// The exact failing case from #1712.
	got := id.NewSystemNodeShortId("proactive", "v1:planner:responsibility:d6ac657f")
	if err := id.ValidateShortId("v1:planner:plan", got); err != nil {
		t.Errorf("#1712 regression: reactive-loop plan id %q invalid: %v", got, err)
	}
	t.Logf("#1712: %d system-generated ids all pass the engine ID validator", n)
}

// --- #2216: dead-logic lint (replaced the #1707 @entrypoint audit) ---

func deadLogicCheck() check {
	return check{
		Issue:   "#2216",
		Dim:     "dead-logic-lint",
		NeedsDB: false,
		Run:     runDeadLogic,
	}
}

// runDeadLogic exercises the dead-logic lint that replaced the retired
// @entrypoint registration audit (ADR §2.4). Every logic must be referenced by
// some logic / query / tool / automation. WARNING phase: orphans are logged,
// not failed (I5 promotes it to a hard gate). It also guards the @entrypoint
// retirement -- the lint API must exist and run cleanly.
func runDeadLogic(t *testing.T, e *Env) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	dead := memql.DeadLogicNames(logger)
	for _, name := range dead {
		t.Logf("#2216 WARNING: dead logic %q -- no logic/query/tool/automation references it", name)
	}
	t.Logf("#2216: dead-logic lint ran; %d orphan(s) (warning window)", len(dead))
}

// --- #1705: LiteralValueNode / bare-literal AST evaluation ---

func literalEvalCheck() check {
	return check{
		Issue: "#1705",
		Dim:   "literal-value-node-eval",
		// Execute routes through the engine read path, which needs a DB handle;
		// gated so a no-DB go-checks run skips rather than fails.
		NeedsDB: true,
		Run:     runLiteralEval,
	}
}

func runLiteralEval(t *testing.T, e *Env) {
	// A bare literal expression reduces to a LiteralValueNode at the plan root.
	// Before #1705 the evaluator returned "unsupported expression node
	// *memql.LiteralValueNode"; after, it returns the scalar.
	res, err := e.Eng.Execute(e.Ctx, `"node.created"`)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported expression node") {
			t.Fatalf("#1705 regression: bare literal failed AST dispatch: %v", err)
		}
		t.Fatalf("#1705: bare-literal Execute errored: %v", err)
	}
	if res == nil {
		t.Fatal("#1705: bare-literal Execute returned nil result")
	}
	got := res.OutputPayload()
	if !literalMatches(got, "node.created") {
		t.Fatalf("#1705: bare literal did not evaluate to its scalar; got %#v", got)
	}
	t.Logf("#1705: bare-literal / LiteralValueNode evaluates to its scalar value")
}

// literalMatches accepts either the bare scalar or a canonical single-element
// array envelope wrapping it (#1710 applies the envelope broadly).
func literalMatches(got any, want string) bool {
	switch v := got.(type) {
	case string:
		return v == want
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
