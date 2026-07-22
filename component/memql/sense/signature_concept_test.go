package sense

// Tests for signatureConceptDiagnostics: a construct whose signature binds a
// concept that resolves to nothing (the user's symptom 5). A configurable fake
// graph drives the tri-state + conservatism; the real dslimports resolver is
// covered end-to-end in component/memql.

import (
	"strings"
	"testing"
)

// a valid mutate body; %s is the signature concept.
func mutateOn(concept string) string {
	return "mutate " + concept + " setThing {\n  args {\n    id  string!\n  }\n  update {\n    id: args.id\n  }\n}\n"
}

func TestSignatureConcept_MissingFlaggedError(t *testing.T) {
	// symptom 5: `full` is bound but exists nowhere and is not imported; the
	// buffer imports fylo-only, so its absence is provable.
	src := "use fylo.concepts.{ order }\n\n" + mutateOn("full")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces: map[string]bool{"fylo": true},
		concepts:   map[string]Resolved{"full": ResolvedNo},
	})
	got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept")
	if len(got) != 1 {
		t.Fatalf("want 1 unknown-signature-concept, got %d: %+v", len(got), s.Diagnose(src, "m.memql"))
	}
	if got[0].Severity != SeverityError {
		t.Errorf("severity = %v, want Error (boot CrashLoops on this)", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, `"full"`) {
		t.Errorf("message should name the concept: %q", got[0].Message)
	}
}

func TestSignatureConcept_ImportedIsSilent(t *testing.T) {
	// The bound concept IS imported -> import checks own it; never double-flag,
	// and an external import is legitimately supplied from outside the tree.
	src := "use fylo.concepts.{ order }\n\n" + mutateOn("order")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces: map[string]bool{"fylo": true},
		concepts:   map[string]Resolved{"order": ResolvedNo}, // even if the graph said No
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("imported signature concept flagged: %+v", got)
	}
}

func TestSignatureConcept_UnimportedButResolvesIsSilent(t *testing.T) {
	// Unimported but unique in the workspace (boot resolves globally) -> silent.
	src := "use fylo.concepts.{ somethingElse }\n\n" + mutateOn("order")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces: map[string]bool{"fylo": true},
		concepts:   map[string]Resolved{"order": ResolvedYes},
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("resolvable signature concept flagged: %+v", got)
	}
}

func TestSignatureConcept_ExternalImportNotProvableSilent(t *testing.T) {
	// The buffer imports an external (unowned) namespace, so an unresolved name
	// could live there -- not provable, stay silent even though ConceptExists=No.
	src := "use platform.mutations.{ stageOutboundRequest }\n\n" + mutateOn("full")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces: map[string]bool{"fylo": true}, // platform NOT owned
		concepts:   map[string]Resolved{"full": ResolvedNo},
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("external-import buffer flagged (not provable): %+v", got)
	}
}

func TestSignatureConcept_NoFormBImportsNotProvableSilent(t *testing.T) {
	// No Form-B imports at all -> no positive evidence the author works
	// import-first -> not provable.
	src := mutateOn("full")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces: map[string]bool{"fylo": true},
		concepts:   map[string]Resolved{"full": ResolvedNo},
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("no-Form-B buffer flagged (not provable): %+v", got)
	}
}

func TestSignatureConcept_ImportsOnlyTreeNotProvableSilent(t *testing.T) {
	src := "use fylo.concepts.{ order }\n\n" + mutateOn("full")
	s := NewWithWorkspace(nil, fakeGraph{
		namespaces:  map[string]bool{"fylo": true},
		concepts:    map[string]Resolved{"full": ResolvedNo},
		importsOnly: true, // an invisible-declarations file exists
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("imports-only tree flagged (not provable): %+v", got)
	}
}

func TestSignatureConcept_RegistryResolvesUnimportedEngineConcept(t *testing.T) {
	// The blocker case: `budget` is a real engine concept (v1:router:budget)
	// bound without an import from a product-only buffer. The product-scoped
	// workspace graph says absent, but the GLOBAL registry -- what boot actually
	// resolves against -- has it, so sense must stay silent.
	src := "use fylo.concepts.{ order }\n\n" + mutateOn("budget")
	reg := &stubRegistry{concepts: map[string]*ConceptInfo{"v1:router:budget": {Name: "v1:router:budget"}}}
	s := NewWithWorkspace(reg, fakeGraph{
		namespaces: map[string]bool{"fylo": true},
		// Both absent from the product graph; only the registry knows budget.
		concepts: map[string]Resolved{"budget": ResolvedNo, "nope": ResolvedNo},
	})
	if got := codesFor(s.Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("engine concept resolvable via the registry was flagged: %+v", got)
	}
	// A genuine typo still flags even with a registry present (not a trailing
	// match of any id).
	src2 := "use fylo.concepts.{ order }\n\n" + mutateOn("nope")
	if got := codesFor(s.Diagnose(src2, "m.memql"), "unknown-signature-concept"); len(got) != 1 {
		t.Fatalf("genuine missing concept not flagged with a registry present: %+v", got)
	}
}

func TestSignatureConcept_NoWorkspaceGraphSilent(t *testing.T) {
	src := "use fylo.concepts.{ order }\n\n" + mutateOn("full")
	if got := codesFor(New(nil).Diagnose(src, "m.memql"), "unknown-signature-concept"); len(got) != 0 {
		t.Fatalf("no-op graph flagged: %+v", got)
	}
}
