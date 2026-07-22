package sense

// Tests for importDiagnostics (diagnose.go): Form-B `use <ns>.<kind>.{ ids }`
// resolution against the workspace graph. A configurable fake graph drives the
// tri-state so the diagnostic logic is exercised in isolation from the real
// dslimports resolver (which is covered in component/memql).

import (
	"strings"
	"testing"
)

// fakeGraph answers module/symbol queries from lookup tables; anything absent is
// Unknown (the inconclusive default), matching the no-op graph.
type fakeGraph struct {
	modules map[string]Resolved // "ns.kind" -> resolved
	symbols map[string]Resolved // "ns.kind.id" -> resolved
}

func (f fakeGraph) ModuleResolves(ns, kind string) Resolved {
	if r, ok := f.modules[ns+"."+kind]; ok {
		return r
	}
	return ResolvedUnknown
}

func (f fakeGraph) SymbolDeclared(ns, kind, id string) Resolved {
	if r, ok := f.symbols[ns+"."+kind+"."+id]; ok {
		return r
	}
	return ResolvedUnknown
}

func (fakeGraph) ConceptExists(string, string) Resolved   { return ResolvedUnknown }
func (fakeGraph) Namespaces() []string                    { return nil }
func (fakeGraph) Kinds(string) []string                   { return nil }
func (fakeGraph) SymbolsInModule(string, string) []string { return nil }

// diagCodes returns the Code of every diagnostic whose message contains want, or
// all codes when want is empty.
func codesFor(diags []Diagnostic, code string) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

const useConstructTail = "\n\nquery order listOrders {\n  filter id == \"x\"\n}\n"

func TestImportDiagnostics_WrongKindSegment(t *testing.T) {
	// symptom 1: `use fylo.concept.{ order }` -- namespace owned, kind is wrong.
	src := "use fylo.concept.{ order }" + useConstructTail
	s := NewWithWorkspace(nil, fakeGraph{
		modules: map[string]Resolved{"fylo.concept": ResolvedNo},
	})
	diags := s.Diagnose(src, "f.memql")
	got := codesFor(diags, "unknown-import-module")
	if len(got) != 1 {
		t.Fatalf("want 1 unknown-import-module diagnostic, got %d: %+v", len(got), diags)
	}
	if !strings.Contains(got[0].Message, `"concept"`) {
		t.Errorf("message should name the bad kind: %q", got[0].Message)
	}
	// The squiggle must land on the kind segment, not the namespace.
	if line := lineOf(src, got[0].Range.Start); !strings.Contains(line, "fylo.concept") {
		t.Errorf("diagnostic anchored on line %q, want the use line", line)
	}
}

func TestImportDiagnostics_UndeclaredId(t *testing.T) {
	// symptom 2/4: `use fylo.concepts.{ oder }` -- module resolves, id does not.
	src := "use fylo.concepts.{ oder }" + useConstructTail
	s := NewWithWorkspace(nil, fakeGraph{
		modules: map[string]Resolved{"fylo.concepts": ResolvedYes},
		symbols: map[string]Resolved{"fylo.concepts.oder": ResolvedNo},
	})
	diags := s.Diagnose(src, "f.memql")
	got := codesFor(diags, "unknown-import-symbol")
	if len(got) != 1 {
		t.Fatalf("want 1 unknown-import-symbol diagnostic, got %d: %+v", len(got), diags)
	}
	if !strings.Contains(got[0].Message, `"oder"`) {
		t.Errorf("message should name the bad id: %q", got[0].Message)
	}
}

func TestImportDiagnostics_ValidImportSilent(t *testing.T) {
	src := "use fylo.concepts.{ order }" + useConstructTail
	s := NewWithWorkspace(nil, fakeGraph{
		modules: map[string]Resolved{"fylo.concepts": ResolvedYes},
		symbols: map[string]Resolved{"fylo.concepts.order": ResolvedYes},
	})
	if diags := s.Diagnose(src, "f.memql"); len(diags) != 0 {
		t.Fatalf("valid import produced diagnostics: %+v", diags)
	}
}

func TestImportDiagnostics_ExternalNamespaceSilent(t *testing.T) {
	// A product bundle imports an engine namespace it does not carry: the graph
	// answers Unknown for everything, so nothing must be flagged.
	src := "use platform.mutations.{ stageOutboundRequest }" + useConstructTail
	s := NewWithWorkspace(nil, fakeGraph{}) // all Unknown
	if diags := s.Diagnose(src, "f.memql"); len(diags) != 0 {
		t.Fatalf("external-namespace import produced diagnostics: %+v", diags)
	}
}

func TestImportDiagnostics_MultiSegmentCapabilitySkipped(t *testing.T) {
	// A consolidated-capability import has 3+ segments; the (ns, kind) graph API
	// cannot faithfully resolve it (it maps to capabilities/capabilities.memql
	// with a dotted prefix), so it must be skipped -- NOT flagged even though a
	// naive 2-arg lookup of ("capabilities","integration") answers ResolvedNo.
	src := "use capabilities.integration.github.{ tagRelease }" + useConstructTail
	s := NewWithWorkspace(nil, fakeGraph{
		modules: map[string]Resolved{"capabilities.integration": ResolvedNo},
	})
	if diags := s.Diagnose(src, "f.memql"); len(diags) != 0 {
		t.Fatalf("multi-segment capability import flagged: %+v", diags)
	}
}

func TestImportDiagnostics_NoWorkspaceGraphSilent(t *testing.T) {
	// The registry-less, graph-less service (New(nil)) must stay silent.
	src := "use fylo.concept.{ oder }" + useConstructTail
	s := New(nil)
	if diags := s.Diagnose(src, "f.memql"); len(diags) != 0 {
		t.Fatalf("no-op graph produced diagnostics: %+v", diags)
	}
}

// lineOf returns the source line containing pos (1-indexed).
func lineOf(source string, pos Position) string {
	lines := strings.Split(source, "\n")
	if pos.Line-1 < 0 || pos.Line-1 >= len(lines) {
		return ""
	}
	return lines[pos.Line-1]
}
