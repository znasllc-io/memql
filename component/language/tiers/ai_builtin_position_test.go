package tiers

// ai_builtin_position_test.go -- WHERE A MODEL CALL MAY BE WRITTEN.
//
// `ai` is a builtin (dsl/agents/builtins.memql), so `builtin ai(...)` is an
// ast.KindConstructCall and the manifest decides it by that kind. This file
// pins the two answers that keep a model call out of a per-row and a per-event
// position, because both failure modes are silent and expensive:
//
//   - In a QUERY FILTER or a SPEC BODY, over the row, it would be one model
//     call per row examined -- so a page of a hundred rows is a hundred calls,
//     charged to whoever happened to open the screen.
//   - In a TRIGGER FILTER or an AUTOMATION CONDITION it fires once per matching
//     event, with no statement to journal it and no run whose ceilings apply.
//
// THE OTHER HALF, and it is the one worth writing down: `ai` must never become
// a CATALOG function. FunctionAdmission answers from functions.Catalog(), and
// the seven allFunctions positions -- the trigger filter among them -- admit
// every catalog function unconditionally. A catalogued `ai` would therefore be
// Admitted in a trigger filter by the manifest's own rule, whatever the kind
// rule says, because it would parse as an ordinary ast.KindCall rather than as
// a construct call. Declaring it as a builtin is what keeps the kind rule the
// one that decides.

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

// TestAiIsNotACatalogFunction is the guard against the tempting "helpful"
// change: adding an `ai` entry to the v1 function catalog so a bare `ai(...)`
// resolves. It would make a model call Admitted at every allFunctions
// position, per event and per row, and the manifest would be right to do it --
// the catalog is the list of things a position may call freely.
func TestAiIsNotACatalogFunction(t *testing.T) {
	for _, f := range functions.Catalog() {
		if f.Key() == "ai" {
			t.Fatal("`ai` is in the v1 function catalog. It is a BUILTIN, declared in " +
				"dsl/agents/builtins.memql, and the distinction is load-bearing: a catalog entry is " +
				"Admitted at every allFunctions position -- a trigger filter included -- so a " +
				"catalogued `ai` is a model call per matching event with no statement journalling it " +
				"and no run whose ceilings apply. A builtin call is a construct call, which the " +
				"kind rules refuse in every position but a body statement.")
		}
	}
	if got := FunctionAdmission(PositionTriggerFilter, "ai"); got != Refused {
		t.Fatalf("FunctionAdmission(triggerFilter, \"ai\") = %v, want %v", got, Refused)
	}
	if got := FunctionAdmission(PositionQueryFilter, "ai"); got != Refused {
		t.Fatalf("FunctionAdmission(queryFilter, \"ai\") = %v, want %v", got, Refused)
	}
}

// TestAModelCallIsABodyStatementAndNothingElse pins the kind rule that decides
// `builtin ai(...)`: admitted in the two positions where an expression sits in
// the statement that journals it, refused everywhere else.
//
// It walks EVERY position rather than naming a few, so a position added later
// has to be classified here rather than inheriting an answer by omission.
func TestAModelCallIsABodyStatementAndNothingElse(t *testing.T) {
	// The only two positions a construct call -- `query ...`, `mutation ...`,
	// `builtin ai(...)` -- belongs in. Both are statement positions.
	statementPositions := map[Position]bool{
		PositionLogicBody:    true,
		PositionStepArgument: true,
	}
	for _, p := range Positions() {
		got := KindAdmission(p, ast.KindConstructCall)
		want := Refused
		if statementPositions[p] {
			want = Admitted
		}
		if got != want {
			t.Errorf("KindAdmission(%q, constructCall) = %v, want %v.\n"+
				"`builtin ai(...)` is a construct call, so this row decides whether a model call may be "+
				"written at %q. A new statement position belongs in statementPositions above; anything "+
				"else admitting a construct call would admit a model call per row or per event.",
				p, got, want, p)
		}
	}
}
