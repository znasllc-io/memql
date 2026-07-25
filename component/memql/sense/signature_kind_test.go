package sense

import (
	"strings"
	"testing"
)

// todosGraph mirrors dsl/todos: the CONCEPT is `todo` (singular) and `todos`
// is a QUERY bound to it. That collision is the whole trap -- the plural reads
// like a concept name.
func todosGraph() fakeGraph {
	return fakeGraph{declSites: map[string][]DeclSite{
		"todo":     {{File: "todos/concepts.memql", Name: "todo", Kind: "concept", Line: 21, Column: 9}},
		"todos":    {{File: "todos/queries.memql", Name: "todos", Kind: "query", Line: 12, Column: 12}},
		"todoFull": {{File: "todos/shapes.memql", Name: "todoFull", Kind: "shape", Line: 6, Column: 12}},
	}}
}

// TestSignatureKind_BindingAQueryIsFlagged is the reported case: `shape`
// binds a CONCEPT, and `todos` is a query. Nothing flagged it, because the
// sibling rule only asks "does this name exist anywhere" -- and it does. The
// author's first sign of trouble was a boot failure.
func TestSignatureKind_BindingAQueryIsFlagged(t *testing.T) {
	s := NewWithWorkspace(nil, todosGraph())
	src := "use todos.queries.{ todos }\n\nshape todos displayTodo {\n}"
	diags := s.Diagnose(src, "todos/shapes.memql")

	var found *Diagnostic
	for i := range diags {
		if diags[i].Code == "signature-binds-wrong-kind" {
			found = &diags[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("binding a query as a shape's concept must be flagged; got codes %v", allCodes(diags))
	}
	if found.Severity != SeverityError {
		t.Errorf("severity = %v, want SeverityError -- this CrashLoops the node at boot", found.Severity)
	}
	if !strings.Contains(found.Message, "todos") || !strings.Contains(found.Message, "query") {
		t.Errorf("message must name the symbol and what it actually is; got %q", found.Message)
	}
	// The squiggle must sit on the bound name, not the construct keyword.
	if found.Range.Start.Line != 3 {
		t.Errorf("diagnostic anchored at line %d, want line 3 (the signature)", found.Range.Start.Line)
	}
}

// TestSignatureKind_ImportingItDoesNotSilenceIt pins the specific hole this
// closes. signatureConceptDiagnostics skips any Form-B imported name
// regardless of the module it came from, so importing the query actively
// suppressed the existing check. The kind rule must not inherit that.
func TestSignatureKind_ImportingItDoesNotSilenceIt(t *testing.T) {
	s := NewWithWorkspace(nil, todosGraph())
	withImport := s.Diagnose("use todos.queries.{ todos }\n\nshape todos displayTodo {\n}", "todos/shapes.memql")
	if !hasCode(withImport, "signature-binds-wrong-kind") {
		t.Error("an explicit import must not suppress the wrong-kind diagnostic")
	}
	noImport := s.Diagnose("shape todos displayTodo {\n}", "todos/shapes.memql")
	if !hasCode(noImport, "signature-binds-wrong-kind") {
		t.Error("the wrong-kind diagnostic must fire with or without the import")
	}
}

// TestSignatureKind_CorrectBindingIsSilent: the fix the author wants to be
// guided to. `todo` IS the concept.
func TestSignatureKind_CorrectBindingIsSilent(t *testing.T) {
	s := NewWithWorkspace(nil, todosGraph())
	diags := s.Diagnose("use todos.concepts.{ todo }\n\nshape todo displayTodo {\n}", "todos/shapes.memql")
	if hasCode(diags, "signature-binds-wrong-kind") {
		t.Errorf("binding the real concept must be silent; got codes %v", allCodes(diags))
	}
}

// TestSignatureKind_UnknownNameIsLeftAlone: a name the workspace has never
// seen may be delivered at runtime through MEMQL_DSL_PATH, so it belongs to
// the missing-concept rule, not this one. Flagging it here would double-report
// and would be wrong for a product bundle.
func TestSignatureKind_UnknownNameIsLeftAlone(t *testing.T) {
	s := NewWithWorkspace(nil, todosGraph())
	diags := s.Diagnose("shape somethingDeliveredAtRuntime proj {\n}", "todos/shapes.memql")
	if hasCode(diags, "signature-binds-wrong-kind") {
		t.Errorf("a name the tree cannot see must not be flagged as wrong-kind; got codes %v", allCodes(diags))
	}
}

// TestSignatureKind_NoWorkspaceIsSilent: with no graph there is nothing to
// prove a binding wrong.
func TestSignatureKind_NoWorkspaceIsSilent(t *testing.T) {
	s := New(nil)
	if hasCode(s.Diagnose("shape todos displayTodo {\n}", "todos/shapes.memql"), "signature-binds-wrong-kind") {
		t.Error("without a workspace graph the rule cannot prove anything and must stay silent")
	}
}

// allCodes lists the diagnostic codes present, for readable failures.
func allCodes(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(diags []Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestCompleteConstructConcept_InScopeRanksFirst pins the ordering half of
// #2762. Every concept in the tree stays on offer -- binding an unimported one
// is legitimate and the import snippet completes it in a keystroke -- but the
// ones the file can bind RIGHT NOW must come first. Before this they all sorted
// identically, so the author scrolled a flat alphabetical wall of ~100 with no
// signal about which two were actually in scope.
func TestCompleteConstructConcept_InScopeRanksFirst(t *testing.T) {
	reg := &stubRegistry{concepts: map[string]*ConceptInfo{
		"v1:agents:agent":           {Name: "v1:agents:agent"},
		"v1:calendar:calendarEvent": {Name: "v1:calendar:calendarEvent"},
		"v1:todos:todo":             {Name: "v1:todos:todo"},
	}}
	s := New(reg)
	// The file lives in calendar/ (so calendarEvent is ambient) and imports
	// agents' concept. `todo` is neither.
	src := "use agents.concepts.{ agent }\n\nshape "
	items := s.Complete(src, 3, 7, "dsl/calendar/shapes.memql")

	priority := map[string]int{}
	for _, it := range items {
		if it.Kind == "concept" {
			priority[it.Label] = it.SortPriority
		}
	}
	for _, name := range []string{"agent", "calendarEvent", "todo"} {
		if _, ok := priority[name]; !ok {
			t.Fatalf("%q must still be offered -- ranking must not remove choices; got %v", name, priority)
		}
	}
	if priority["agent"] >= priority["todo"] {
		t.Errorf("an IMPORTED concept must rank above an unimported one: agent=%d todo=%d", priority["agent"], priority["todo"])
	}
	if priority["calendarEvent"] >= priority["todo"] {
		t.Errorf("an AMBIENT same-domain concept must rank above an unimported one: calendarEvent=%d todo=%d", priority["calendarEvent"], priority["todo"])
	}
}
