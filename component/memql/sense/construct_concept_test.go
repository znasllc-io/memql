package sense

import (
	"reflect"
	"strings"
	"testing"
)

// fakeRegistry is a minimal RegistryProvider stub for the construct-concept
// completion tests. It only models the concept surface the tests exercise; all
// other registry methods return empty.
type fakeRegistry struct {
	concepts []string
}

func (f *fakeRegistry) FunctionNames() []string                  { return nil }
func (f *fakeRegistry) FunctionGet(string) (*FunctionInfo, bool) { return nil, false }
func (f *fakeRegistry) ConceptNames() []string                   { return f.concepts }
func (f *fakeRegistry) ConceptGet(name string) (*ConceptInfo, bool) {
	for _, c := range f.concepts {
		if c == name {
			return &ConceptInfo{Name: name, Description: "concept " + name,
				Fields: []FieldInfo{{Name: "title", Type: "string", Description: "probe field"}}}, true
		}
	}
	return nil, false
}
func (f *fakeRegistry) SpecNames() []string                      { return nil }
func (f *fakeRegistry) ToolNames() []string                      { return nil }
func (f *fakeRegistry) ToolGet(string) (*ToolInfo, bool)         { return nil, false }
func (f *fakeRegistry) PromptNames() []string                    { return nil }
func (f *fakeRegistry) PromptGet(string) (*PromptInfo, bool)     { return nil, false }
func (f *fakeRegistry) ProviderNames() []string                  { return nil }
func (f *fakeRegistry) ProviderGet(string) (*ProviderInfo, bool) { return nil, false }
func (f *fakeRegistry) ShapeNames() []string                     { return nil }
func (f *fakeRegistry) ShapeGet(string) (*ShapeInfo, bool)       { return nil, false }
func (f *fakeRegistry) IntegrationCapabilities() []string        { return nil }

// completionLabels collects completion labels at the end of the given source.
// The cursor is placed at the end of the (single-line or multi-line) source.
func completionLabels(t *testing.T, rp RegistryProvider, source string) []CompletionItem {
	t.Helper()
	s := New(rp)
	// Cursor at the very end of the source.
	line := 1
	col := len(source) + 1
	// For multi-line sources, place the cursor at the end of the last line.
	last := source
	for i := 0; i < len(source); i++ {
		if source[i] == '\n' {
			line++
		}
	}
	if line > 1 {
		// recompute last-line length
		from := 0
		for i := 0; i < len(source); i++ {
			if source[i] == '\n' {
				from = i + 1
			}
		}
		last = source[from:]
		col = len(last) + 1
	}
	return s.Complete(source, line, col, "test.memql")
}

func hasLabel(items []CompletionItem, label string) bool {
	for _, it := range items {
		if it.Label == label {
			return true
		}
	}
	return false
}

// The real RegistryProvider.ConceptNames() returns CANONICAL ids
// (v1:<domain>:<leaf>), so these tests model that; completion derives the
// short signature name (`space`) and the import domain (`cognition`) from it.
const (
	cidSpace = "v1:cognition:space"
	cidAgent = "v1:agents:agent"
)

// TestConstructConceptAfterMutateOffersConcept verifies the headline path:
// after `mutate ` the bound concept's SHORT name `space` is offered (not the
// canonical id the registry stores). `mutate` is the declaration keyword
// (memql#2041); `mutation` is the invocation-step prefix only.
func TestConstructConceptAfterMutateOffersConcept(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace}}
	items := completionLabels(t, rp, "mutate ")
	if !hasLabel(items, "space") {
		t.Fatalf("after `mutate ` expected short concept `space` offered; got %v", labelsOf(items))
	}
	if hasLabel(items, cidSpace) {
		t.Errorf("should offer the SHORT name `space`, not the canonical id %q", cidSpace)
	}
	for _, it := range items {
		if it.Label == "space" && it.Kind != "concept" {
			t.Errorf("`space` should be a concept completion, got kind %q", it.Kind)
		}
	}
}

// TestConstructConceptAfterQueryOffersConcept verifies the same for `query `.
func TestConstructConceptAfterQueryOffersConcept(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace}}
	items := completionLabels(t, rp, "query ")
	if !hasLabel(items, "space") {
		t.Fatalf("after `query ` expected short concept `space` offered; got %v", labelsOf(items))
	}
}

// TestConstructConceptPrefixFilter confirms the partial SHORT name filters.
func TestConstructConceptPrefixFilter(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace, cidAgent}}
	items := completionLabels(t, rp, "mutate sp")
	if !hasLabel(items, "space") {
		t.Fatalf("prefix `sp` should offer `space`; got %v", labelsOf(items))
	}
	if hasLabel(items, "agent") {
		t.Errorf("prefix `sp` should not offer `agent`; got %v", labelsOf(items))
	}
}

// TestConstructConceptImportSuggestedWhenMissing verifies the import-suggestion
// fires with the DOMAIN-qualified path when the concept is NOT imported.
func TestConstructConceptImportSuggestedWhenMissing(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace}}
	items := completionLabels(t, rp, "mutate ")
	if !hasLabel(items, "use cognition.concepts.{ space }") {
		t.Fatalf("expected a domain-qualified import suggestion for un-imported `space`; got %v", labelsOf(items))
	}
}

// TestConstructConceptImportSuppressedWhenImported verifies the import
// suggestion is suppressed when the concept's SHORT name is already in scope.
func TestConstructConceptImportSuppressedWhenImported(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace}}
	src := "use cognition.concepts.{ space }\nmutate "
	items := completionLabels(t, rp, src)
	if !hasLabel(items, "space") {
		t.Fatalf("concept `space` should still be offered; got %v", labelsOf(items))
	}
	if hasLabel(items, "use cognition.concepts.{ space }") {
		t.Errorf("import suggestion should be suppressed when `space` is already imported; got %v", labelsOf(items))
	}
}

// TestConstructConceptQueryImportToggle exercises the query path both ways.
func TestConstructConceptQueryImportToggle(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{cidSpace}}

	missing := completionLabels(t, rp, "query ")
	if !hasLabel(missing, "use cognition.concepts.{ space }") {
		t.Errorf("query: expected import suggestion when not imported; got %v", labelsOf(missing))
	}

	imported := completionLabels(t, rp, "use cognition.concepts.{ space }\nquery ")
	if hasLabel(imported, "use cognition.concepts.{ space }") {
		t.Errorf("query: import suggestion should be suppressed when imported; got %v", labelsOf(imported))
	}
}

// TestConstructConceptNotFiredInBody confirms the construct-concept context does
// NOT hijack completion inside a body where these words mean something else.
func TestConstructConceptNotFiredInBody(t *testing.T) {
	// Inside an unmatched brace -- should not be ContextConstructConcept.
	ctx := analyzeCursorContext("mutate space mk {\n  mutate ", 2, 9)
	if ctx.Kind == ContextConstructConcept {
		t.Errorf("construct-concept context must not fire inside a body block")
	}
}

func labelsOf(items []CompletionItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

// TestConstructConceptImportSuppressedForSameDomain pins the #2617
// ambient-scope carve-out: a concept of the file's OWN domain needs no
// import (the loader seeds the namespace hint from the directory), so
// the "use ..." import suggestion must not fire -- while a cross-domain
// concept still gets one.
func TestConstructConceptImportSuppressedForSameDomain(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"v1:planner:plan", "v1:cognition:space"}}
	s := New(rp)
	src := "mutate pla"
	items := s.Complete(src, 1, len(src)+1, "dsl/planner/mutations.memql")
	for _, it := range items {
		if it.Kind == "snippet" && it.Label == "use planner.concepts.{ plan }" {
			t.Errorf("same-domain concept must not get an import suggestion, got %+v", it)
		}
	}

	src = "mutate spa"
	items = s.Complete(src, 1, len(src)+1, "dsl/planner/mutations.memql")
	found := false
	for _, it := range items {
		if it.Kind == "snippet" && it.Label == "use cognition.concepts.{ space }" {
			found = true
		}
	}
	if !found {
		t.Error("cross-domain concept must still get the import suggestion")
	}
}

// TestConceptImportInsertsOnlyTheName: at the concept slot the import
// suggestion inserts the concept's NAME -- the `use` line is an edit of its
// own, with the file's imports (memql#5359). It inserted the whole
// `use <domain>.concepts.{ <name> }` line at the slot, which made
// `query use cognition.concepts.{ space }` -- a file that does not parse.
func TestConceptImportInsertsOnlyTheName(t *testing.T) {
	s := New(&fakeRegistry{concepts: []string{"v1:cognition:space"}})
	for _, src := range []string{"query ", "mutate ", "seed ", "shape "} {
		var found bool
		for _, it := range s.Complete(src, 1, len(src)+1, "probe.memql") {
			if it.Label != "use cognition.concepts.{ space }" {
				continue
			}
			found = true
			if it.InsertText != "space" {
				t.Errorf("%q: the import suggestion inserts %q at the concept slot, want the name alone", src, it.InsertText)
			}
		}
		if !found {
			t.Errorf("%q: no import suggestion for an unimported concept", src)
		}
	}
}

// TestConceptImportEditGoesWithTheFileImports: the import suggestion's `use`
// line is an insertion after the file's last `use` declaration -- past the
// closing brace of one whose list spans lines -- or at the top of a file with
// none, followed by a blank line.
func TestConceptImportEditGoesWithTheFileImports(t *testing.T) {
	s := New(&fakeRegistry{concepts: []string{"v1:cognition:space"}})
	for _, tc := range []struct {
		src  string
		at   Position
		text string
	}{
		{"query ", Position{Line: 1, Column: 1}, "use cognition.concepts.{ space }\n\n"},
		{"use cluster.concepts.{ node }\nquery ", Position{Line: 2, Column: 1}, "use cognition.concepts.{ space }\n"},
		{"use cluster.concepts.{\n  node,\n  row\n}\n\nquery ", Position{Line: 5, Column: 1}, "use cognition.concepts.{ space }\n"},
	} {
		lines := strings.Split(tc.src, "\n")
		var edits []TextEdit
		for _, it := range s.Complete(tc.src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
			if it.Label == "use cognition.concepts.{ space }" {
				edits = it.AdditionalEdits
			}
		}
		want := []TextEdit{{Range: Range{Start: tc.at, End: tc.at}, NewText: tc.text}}
		if !reflect.DeepEqual(edits, want) {
			t.Errorf("%q: the import edit is %+v, want %+v", tc.src, edits, want)
		}
	}
}
