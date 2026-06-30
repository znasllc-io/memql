package dslspec

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// TestBuildNonEmpty is a smoke test: every section of the spec is populated.
func TestBuildNonEmpty(t *testing.T) {
	s := Build()
	if s.Version != SpecVersion {
		t.Fatalf("Version = %q, want %q", s.Version, SpecVersion)
	}
	checks := map[string]int{
		"constructs":  len(s.Constructs),
		"annotations": len(s.Annotations),
		"keywords":    len(s.Keywords),
		"operators":   len(s.Operators),
		"fieldTypes":  len(s.FieldTypes),
		"nextRules":   len(s.NextRules),
	}
	for name, n := range checks {
		if n == 0 {
			t.Errorf("spec section %q is empty", name)
		}
	}
}

// TestConstructsCoverLiveGrammar pins the author-facing construct set. If the
// grammar gains or loses a top-level construct, this list must change in
// lockstep (the #2124 drift test additionally cross-checks the parser).
func TestConstructsCoverLiveGrammar(t *testing.T) {
	want := map[string]bool{
		"concept": true, "query": true, "mutation": true, "logic": true,
		"automation": true, "spec": true, "trait": true, "shape": true,
		"tool": true, "prompt": true, "provider": true, "builtin": true,
		"policy": true, "seed": true, "use": true, "action": true,
		"capability": true,
	}
	got := map[string]bool{}
	for _, c := range Build().Constructs {
		if c.Keyword == "" {
			t.Error("construct with empty keyword")
		}
		if c.Doc == "" {
			t.Errorf("construct %q has no doc", c.Keyword)
		}
		got[c.Keyword] = true
	}
	for kw := range want {
		if !got[kw] {
			t.Errorf("missing construct keyword %q", kw)
		}
	}
	for kw := range got {
		if !want[kw] {
			t.Errorf("unexpected construct keyword %q (update the test if the grammar changed)", kw)
		}
	}
}

// TestRetiredGrammarFormsAbsent guards that retired surfaces never creep back
// into the spec: the `has` membership operator (#971), the receiver-function
// model keyword `func` as a construct, and `array` as a non-deprecated type.
func TestRetiredGrammarFormsAbsent(t *testing.T) {
	s := Build()
	for _, op := range s.Operators {
		if op.Symbol == "has" {
			t.Error("retired `has` operator present in spec; membership is `in` only (#971)")
		}
	}
	for _, c := range s.Constructs {
		if c.Keyword == "func" {
			t.Error("`func` is the internal rewriter target, not an author-facing construct")
		}
	}
	for _, ft := range s.FieldTypes {
		if ft.Name == "array" && !ft.Deprecated {
			t.Error("`array` must be marked Deprecated (migrate to []T)")
		}
	}
}

// TestRegistryBackedConstructsResolve asserts that every construct claiming
// RegistryBacked actually resolves in the annotations registry, and -- the
// inverse -- that a construct's RegistryBacked flag is honest. This keeps the
// spec a faithful projection of component/language/annotations.
func TestRegistryBackedConstructsResolve(t *testing.T) {
	for _, c := range Build().Constructs {
		_, inRegistry := annotations.ByReceiver[c.AnnotationReceiver]
		// The concept (empty receiver) and use (import) legitimately key on
		// "" -- only the concept claims RegistryBacked. use is not backed.
		if c.RegistryBacked && !inRegistry {
			t.Errorf("construct %q claims RegistryBacked but receiver %q is not in annotations.ByReceiver",
				c.Keyword, c.AnnotationReceiver)
		}
		if !c.RegistryBacked && inRegistry && c.AnnotationReceiver != "" {
			t.Errorf("construct %q has registry-backed receiver %q but RegistryBacked=false",
				c.Keyword, c.AnnotationReceiver)
		}
	}
}

// TestAnnotationsAreProjectionOfRegistry verifies buildAnnotations is a strict
// projection: every annotation name in the registry appears exactly once with
// its registry doc, and its Receivers map to real construct keywords.
func TestAnnotationsAreProjectionOfRegistry(t *testing.T) {
	s := Build()

	// Every registry annotation name is represented.
	wantNames := map[string]bool{}
	for _, names := range annotations.ByReceiver {
		for _, n := range names {
			wantNames[n] = true
		}
	}
	gotNames := map[string]bool{}
	constructKeywords := map[string]bool{}
	for _, c := range s.Constructs {
		constructKeywords[c.Keyword] = true
	}
	for _, a := range s.Annotations {
		if gotNames[a.Name] {
			t.Errorf("annotation %q appears more than once", a.Name)
		}
		gotNames[a.Name] = true
		if a.Doc != annotations.Docs[a.Name] {
			t.Errorf("annotation %q doc diverges from the registry", a.Name)
		}
		if len(a.Receivers) == 0 {
			t.Errorf("annotation %q has no receivers", a.Name)
		}
		for _, r := range a.Receivers {
			if !constructKeywords[r] {
				t.Errorf("annotation %q lists receiver %q which is not a construct keyword", a.Name, r)
			}
		}
	}
	for n := range wantNames {
		if !gotNames[n] {
			t.Errorf("registry annotation %q missing from spec", n)
		}
	}
}

// TestImportSuggestionRulePresent guards the owner-requested behaviour: after
// `mutation`/`query` the spec must mark that a concept is expected and an
// import should be offered when none is in scope.
func TestImportSuggestionRulePresent(t *testing.T) {
	s := Build()
	need := map[string]bool{"afterMutationKeyword": false, "afterQueryKeyword": false}
	for _, r := range s.NextRules {
		if _, ok := need[r.Context]; ok {
			if !r.SuggestImportWhenMissing {
				t.Errorf("rule %q must set SuggestImportWhenMissing", r.Context)
			}
			expectsConcept := false
			for _, e := range r.Expect {
				if e == "concept" {
					expectsConcept = true
				}
			}
			if !expectsConcept {
				t.Errorf("rule %q must expect a concept", r.Context)
			}
			need[r.Context] = true
		}
	}
	for ctx, found := range need {
		if !found {
			t.Errorf("missing next-rule context %q", ctx)
		}
	}
}

// TestJSONRoundTrips confirms the serialized spec is stable through a
// marshal/unmarshal cycle -- the contract the export layer (#2125) depends on.
func TestJSONRoundTrips(t *testing.T) {
	s := Build()
	raw, err := s.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var back Spec
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Version != s.Version {
		t.Errorf("version did not round-trip: %q != %q", back.Version, s.Version)
	}
	if len(back.Constructs) != len(s.Constructs) {
		t.Errorf("construct count changed across round-trip: %d != %d", len(back.Constructs), len(s.Constructs))
	}
	if c := back.ConstructByKeyword("mutation"); c == nil || !c.ConceptInSignature {
		t.Error("mutation construct did not survive round-trip with ConceptInSignature")
	}
}
