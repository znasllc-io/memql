package automations

// trigger_filter_construct_call_test.go -- memql#5582's load check: the tier
// manifest refuses a construct call at tiers.PositionTriggerFilter
// (component/language/tiers/manifest.go's inProcessKinds), and until this
// check existed, nothing enforced that at load. The filter loaded clean,
// memqllint said nothing, and the refusal (construct_call_not_allowed) only
// arrived once the trigger fired, on every matching event, because
// evaluateTriggerFilterV1 installs no memql.EvalOptions.Calls hook.
// checkTriggerFilterAdmission (expressions_v1.go) closes that: it runs
// memql.CheckPositionAdmission against the trigger's parsed lambda inside
// triggerFilter(), the same preparer trigger_filter_condition_test.go's D8
// check runs in.

import (
	"errors"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// TestTriggerFilterConstructCallRefusedAtLoad: a trigger filter that calls a
// builtin -- the issue's own repro shape -- is refused at LOAD, through both
// paths PrepareExpressions runs from: an authored file (compileMemQL) and
// compiled JSON (parseJSON, the LogicRunner's body-compile path). The
// refusal names the file (the automation's source path, "test:onRow"), the
// automation, the refused construct as written, the position, and the fix --
// and it echoes EvalCondition's own construct_call_not_allowed wording
// ("cannot be called from this position") so a reader who has seen the
// fire-time refusal recognises this one.
func TestTriggerFilterConstructCallRefusedAtLoad(t *testing.T) {
	loader := NewLoader(LoaderOptions{Registry: conditionCheckRegistry(t)})
	const wantSubstr = "`builtin someBuiltin(x: 1)` does not lower in a trigger filter: cannot be called from this position: a construct call is the work of a statement, and a trigger filter is not one -- a call there would read or write with no step of its own. Move the call into a step of the automation's body [lower_refused]"

	_, err := loader.compileMemQL(filteredAutomation(conditionTicketID, `row => builtin someBuiltin(x: 1)`), "test:onRow")
	if err == nil {
		t.Fatalf("a trigger filter calling a builtin loaded; want the load refusal")
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("got %v\nwant it to contain %q", err, wantSubstr)
	}
	if !strings.Contains(err.Error(), `automation "onRow"`) {
		t.Fatalf("got %v\nwant it to name the automation", err)
	}
	var le *memql.LowerError
	if !errors.As(err, &le) {
		t.Fatalf("the refusal is not a LowerError: %v", err)
	}
	if got := le.RuleCode(); got != memql.LowerCodeRefused {
		t.Fatalf("rule code = %q, want %q", got, memql.LowerCodeRefused)
	}
	if !le.Span.IsZero() {
		// Same reason as checkTriggerFilterFields (D8): the lambda is
		// reparsed from the compiled filter text, not the author's file, so
		// a span here would point an authoring diagnostic at the wrong
		// column. File attribution comes from the automation's source path
		// ("test:onRow" above; the tree loader passes the real file), not
		// from this node's own position.
		t.Fatalf("the refusal carries a span of the compiled filter text (%+v), which an authoring diagnostic would place in the author's file", le.Span)
	}

	// The compiled-JSON path (the LogicRunner's body compile) prepares
	// through the same preparer and refuses the same filter -- the
	// bug's OTHER load path: an automation authored once, from then on
	// loaded from compiled JSON.
	_, err = loader.parseJSON([]byte(`{"name":"onRow","trigger":{"event":"graph.node.created.`+conditionTicketID+`","filter":"row => builtin someBuiltin(x: 1)"},"steps":[{"id":"s","type":"function","function":{"name":"f"}}]}`), "test:v1")
	if err == nil || !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("parseJSON: want the load refusal, got %v", err)
	}
}

// TestTriggerFilterConstructCallRefusedWhenNested proves the check walks the
// WHOLE filter, not only a bare `row => <call>` body: a construct call
// nested inside a comparison and an `||` is refused before any argument of
// the call is evaluated, matching EvalExpr's own construct_call_not_allowed
// contract (checked before any argument is evaluated) at load instead of at
// fire time.
func TestTriggerFilterConstructCallRefusedWhenNested(t *testing.T) {
	loader := NewLoader(LoaderOptions{Registry: conditionCheckRegistry(t)})
	_, err := loader.compileMemQL(filteredAutomation(conditionTicketID, `row => row.archived == true || query someQuery() != nil`), "test:onRow")
	if err == nil {
		t.Fatalf("a trigger filter calling a query inside a comparison loaded; want the load refusal")
	}
	if !strings.Contains(err.Error(), "`query someQuery()` does not lower in a trigger filter") {
		t.Fatalf("got %v\nwant it naming `query someQuery()`", err)
	}
}

// TestTriggerFilterLoadsWithoutAConstructCall is the negative control: the
// node kinds an ordinary trigger filter uses -- a function call, a method
// call, list membership, a ternary -- are untouched by the new check, which
// refuses exactly one node kind (tiers.KindConstructCall) and nothing else.
func TestTriggerFilterLoadsWithoutAConstructCall(t *testing.T) {
	loader := NewLoader(LoaderOptions{Registry: conditionCheckRegistry(t)})
	for name, filter := range map[string]string{
		"a function call":    `row => lower(row.title) == "x"`,
		"a method call":      `row => row.title.includes("x")`,
		"list membership":    `row => row.priority in [1, 2, 3]`,
		"a ternary":          `row => row.archived ? row.priority > 5 : row.priority < 5`,
		"args, now, config":  `row => row.archived == true && args.limit > 0 && now != nil && config.flag == true`,
		"the shipped filter": `row => row.status == "stored"`,
	} {
		t.Run(name, func(t *testing.T) {
			a, err := loader.compileMemQL(filteredAutomation(conditionTicketID, filter), "test:onRow")
			if err != nil {
				t.Fatalf("@filter(%s): %v", filter, err)
			}
			if a.Trigger == nil || a.Trigger.FilterLambda == nil {
				t.Fatalf("@filter(%s): the filter was not prepared into a lambda", filter)
			}
		})
	}
}

// TestTriggerFilterConstructCallStillRefusedAtFireTime is the backstop check
// (memql#5582): EvalCondition must stay fail-closed on a construct call
// regardless of the new load check. It builds an automation whose filter
// loads clean (no construct call), then overwrites Trigger.FilterLambda
// directly with a lambda ParseV1Lambda alone can produce -- bypassing
// triggerFilter()'s admission check the way a filter would if it somehow
// reached the evaluator without going through PrepareExpressions -- and
// confirms the fire-time evaluator still refuses it
// (construct_call_not_allowed), unmodified by this change.
func TestTriggerFilterConstructCallStillRefusedAtFireTime(t *testing.T) {
	a := loadV1(t, `{
		"name": "onRow",
		"trigger": {
			"event": "graph.node.created.`+conditionTicketID+`",
			"filter": "row => row.archived == true"
		},
		"steps": [{"id": "s", "type": "function", "function": {"name": "f"}}]
	}`)
	if !a.exprsPrepared || a.Trigger.FilterLambda == nil {
		t.Fatalf("setup: the harmless filter did not load prepared: %+v", a.Trigger)
	}

	// Bypass the load check the way a filter would if it reached the
	// evaluator without going through triggerFilter(): parse the lambda
	// directly and splice it in after preparation.
	bypassed, err := languageParser.ParseV1Lambda(`row => builtin someBuiltin(x: 1)`)
	if err != nil {
		t.Fatalf("parse the bypass lambda: %v", err)
	}
	a.Trigger.FilterLambda = bypassed

	ev := graphCreatedEvent(conditionTicketID, "t1", map[string]any{"title": "x"})
	fired, err := evaluateTriggerFilter(a, &ev, nil)
	if err == nil {
		t.Fatalf("a construct call reached EvalCondition and fired=%v with no error; the fail-closed backstop is gone", fired)
	}
	var ee *memql.ExprError
	if !errors.As(err, &ee) {
		t.Fatalf("the fire-time refusal is not a memql.ExprError: %v", err)
	}
	if ee.Code != "construct_call_not_allowed" {
		t.Fatalf("code = %q, want %q", ee.Code, "construct_call_not_allowed")
	}
	if !strings.Contains(err.Error(), "builtin someBuiltin(...) cannot be called from this position") {
		t.Fatalf("got %v\nwant it to contain the evaluator's own wording", err)
	}
}
