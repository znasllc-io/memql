package sense

import "testing"

// fakeRegistry is a minimal RegistryProvider stub for the construct-concept
// completion tests. It only models the concept surface the tests exercise; all
// other registry methods return empty.
type fakeRegistry struct {
	concepts []string
}

func (f *fakeRegistry) FunctionNames() []string                 { return nil }
func (f *fakeRegistry) FunctionGet(string) (*FunctionInfo, bool) { return nil, false }
func (f *fakeRegistry) ConceptNames() []string                  { return f.concepts }
func (f *fakeRegistry) ConceptGet(name string) (*ConceptInfo, bool) {
	for _, c := range f.concepts {
		if c == name {
			return &ConceptInfo{Name: name, Description: "concept " + name}, true
		}
	}
	return nil, false
}
func (f *fakeRegistry) SpecNames() []string                    { return nil }
func (f *fakeRegistry) ToolNames() []string                    { return nil }
func (f *fakeRegistry) ToolGet(string) (*ToolInfo, bool)       { return nil, false }
func (f *fakeRegistry) PromptNames() []string                  { return nil }
func (f *fakeRegistry) PromptGet(string) (*PromptInfo, bool)   { return nil, false }
func (f *fakeRegistry) ProviderNames() []string                { return nil }
func (f *fakeRegistry) ProviderGet(string) (*ProviderInfo, bool) { return nil, false }
func (f *fakeRegistry) ShapeNames() []string                   { return nil }
func (f *fakeRegistry) ShapeGet(string) (*ShapeInfo, bool)     { return nil, false }
func (f *fakeRegistry) IntegrationCapabilities() []string      { return nil }

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

// TestConstructConceptAfterMutationOffersConcept verifies the headline path:
// after `mutation ` the bound concept `space` is offered.
func TestConstructConceptAfterMutationOffersConcept(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space"}}
	items := completionLabels(t, rp, "mutation ")
	if !hasLabel(items, "space") {
		t.Fatalf("after `mutation ` expected concept `space` offered; got %v", labelsOf(items))
	}
	// The concept suggestion is the highest-priority item.
	for _, it := range items {
		if it.Label == "space" && it.Kind != "concept" {
			t.Errorf("`space` should be a concept completion, got kind %q", it.Kind)
		}
	}
}

// TestConstructConceptAfterQueryOffersConcept verifies the same for `query `.
func TestConstructConceptAfterQueryOffersConcept(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space"}}
	items := completionLabels(t, rp, "query ")
	if !hasLabel(items, "space") {
		t.Fatalf("after `query ` expected concept `space` offered; got %v", labelsOf(items))
	}
}

// TestConstructConceptPrefixFilter confirms the partial concept name filters.
func TestConstructConceptPrefixFilter(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space", "agent"}}
	items := completionLabels(t, rp, "mutation sp")
	if !hasLabel(items, "space") {
		t.Fatalf("prefix `sp` should offer `space`; got %v", labelsOf(items))
	}
	if hasLabel(items, "agent") {
		t.Errorf("prefix `sp` should not offer `agent`; got %v", labelsOf(items))
	}
}

// TestConstructConceptImportSuggestedWhenMissing verifies the import-suggestion
// fires when the concept is NOT imported in the source.
func TestConstructConceptImportSuggestedWhenMissing(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space"}}
	items := completionLabels(t, rp, "mutation ")
	if !hasLabel(items, "use ...concepts.{ space }") {
		t.Fatalf("expected an import suggestion for un-imported concept `space`; got %v", labelsOf(items))
	}
}

// TestConstructConceptImportSuppressedWhenImported verifies the import
// suggestion is suppressed when the concept IS already in file scope.
func TestConstructConceptImportSuppressedWhenImported(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space"}}
	src := "use cognition.concepts.{ space }\nmutation "
	items := completionLabels(t, rp, src)
	if !hasLabel(items, "space") {
		t.Fatalf("concept `space` should still be offered; got %v", labelsOf(items))
	}
	if hasLabel(items, "use ...concepts.{ space }") {
		t.Errorf("import suggestion should be suppressed when `space` is already imported; got %v", labelsOf(items))
	}
}

// TestConstructConceptQueryImportToggle exercises the query path both ways.
func TestConstructConceptQueryImportToggle(t *testing.T) {
	rp := &fakeRegistry{concepts: []string{"space"}}

	missing := completionLabels(t, rp, "query ")
	if !hasLabel(missing, "use ...concepts.{ space }") {
		t.Errorf("query: expected import suggestion when not imported; got %v", labelsOf(missing))
	}

	imported := completionLabels(t, rp, "use cognition.concepts.{ space }\nquery ")
	if hasLabel(imported, "use ...concepts.{ space }") {
		t.Errorf("query: import suggestion should be suppressed when imported; got %v", labelsOf(imported))
	}
}

// TestConstructConceptNotFiredInBody confirms the construct-concept context does
// NOT hijack completion inside a body where these words mean something else.
func TestConstructConceptNotFiredInBody(t *testing.T) {
	// Inside an unmatched brace -- should not be ContextConstructConcept.
	ctx := analyzeCursorContext("mutation space mk {\n  mutation ", 2, 11)
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
