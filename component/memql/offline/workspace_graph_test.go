package offline_test

import (
	"testing"
	"testing/fstest"

	memql "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// The go-blind fix: when a workspace construct trips the strict-boot gate,
// BuildOfflineSense still returns a service carrying the workspace graph, so
// reference resolution keeps working on exactly the broken workspace -- where
// the old code fell back to a graph-less New(nil).
func TestBuildOfflineSense_GraphSurvivesInitFailure(t *testing.T) {
	root := fstest.MapFS{
		"demo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
concept item {
  id  string  @required
}`)},
		"demo/mutations.memql": &fstest.MapFile{Data: []byte(`use demo.concepts.{ item }

mutation ghostConcept touchGhost {
  args { id  string!  }
  update {
    id: args.id
  }
}`)},
	}

	svc, err := memql.BuildOfflineSense(withLanguageLines(root))
	if err == nil {
		t.Fatal("expected a strict-boot error for a mutation bound to a nonexistent concept")
	}
	if svc == nil {
		t.Fatal("BuildOfflineSense returned a nil service on init failure -- the workspace graph was lost")
	}
	if got := svc.Workspace().ModuleResolves("demo", "concepts"); got != sense.ResolvedYes {
		t.Errorf("graph lost on init failure: ModuleResolves(demo, concepts) = %v, want ResolvedYes", got)
	}
}

// End-to-end: the REAL workspace graph (not a fake) drives Diagnose's import
// checks. Proves #2729's graph and #2730's importDiagnostics compose, and that a
// valid import over a real tree produces zero import diagnostics.
func TestBuildOfflineSense_ImportDiagnosticsEndToEnd(t *testing.T) {
	root := fstest.MapFS{
		"demo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
concept item {
  id  string  @required
}`)},
	}
	svc, err := memql.BuildOfflineSense(withLanguageLines(root))
	if err != nil {
		t.Fatalf("BuildOfflineSense over a clean workspace: %v", err)
	}

	importCodes := func(src string) map[string]int {
		out := map[string]int{}
		for _, d := range svc.Diagnose(src, "q.memql") {
			if d.Code == "unknown-import-module" || d.Code == "unknown-import-symbol" {
				out[d.Code]++
			}
		}
		return out
	}
	tail := "\n\nquery item listItems {\n  filter row => row.id == \"x\"\n}\n"

	// Valid import: zero import diagnostics.
	if got := importCodes("use demo.concepts.{ item }" + tail); len(got) != 0 {
		t.Errorf("valid import flagged: %v", got)
	}
	// Wrong kind segment: one unknown-import-module.
	if got := importCodes("use demo.concept.{ item }" + tail); got["unknown-import-module"] != 1 {
		t.Errorf("wrong-kind import: got %v, want 1 unknown-import-module", got)
	}
	// Undeclared id: one unknown-import-symbol.
	if got := importCodes("use demo.concepts.{ ghost }" + tail); got["unknown-import-symbol"] != 1 {
		t.Errorf("undeclared id: got %v, want 1 unknown-import-symbol", got)
	}
	// External engine namespace absent from this workspace: silent.
	if got := importCodes("use platform.mutations.{ stageOutboundRequest }" + tail); len(got) != 0 {
		t.Errorf("external-namespace import flagged: %v", got)
	}
}

// End-to-end: the REAL workspace graph drives Diagnose's signature-concept check
// (symptom 5). A mutation bound to a concept that exists nowhere -- with the
// buffer importing workspace-only -- is flagged; the same bound to a real,
// imported concept is silent.
func TestBuildOfflineSense_SignatureConceptEndToEnd(t *testing.T) {
	root := fstest.MapFS{
		"demo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
concept item {
  id  string  @required
}`)},
	}
	svc, err := memql.BuildOfflineSense(withLanguageLines(root))
	if err != nil {
		t.Fatalf("BuildOfflineSense: %v", err)
	}
	sigErrs := func(src string) int {
		n := 0
		for _, d := range svc.Diagnose(src, "m.memql") {
			if d.Code == "unknown-signature-concept" {
				n++
			}
		}
		return n
	}
	body := " setThing {\n  args {\n    id  string!\n  }\n  update {\n    id: args.id\n  }\n}\n"

	// Bound to a nonexistent concept, buffer imports demo-only (provable): flagged.
	if got := sigErrs("use demo.concepts.{ item }\n\nmutation full" + body); got != 1 {
		t.Errorf("nonexistent signature concept: got %d unknown-signature-concept, want 1", got)
	}
	// Bound to the real, imported concept: silent.
	if got := sigErrs("use demo.concepts.{ item }\n\nmutation item" + body); got != 0 {
		t.Errorf("valid signature concept flagged: got %d", got)
	}
	// Bound to a real ENGINE concept WITHOUT an import: the global registry
	// (embedded core) resolves it by trailing segment, so boot binds it and sense
	// must stay silent -- the adversarial-review blocker.
	if got := sigErrs("use demo.concepts.{ item }\n\nmutation user" + body); got != 0 {
		t.Errorf("unimported engine concept (registry-resolvable) flagged: got %d", got)
	}
}

// End-to-end: the REAL workspace graph drives segment-aware `use`-line
// completion (#2732) -- `use demo.` offers the module kinds, and the brace list
// offers the module's declared ids, over a real tree.
func TestBuildOfflineSense_UseCompletionEndToEnd(t *testing.T) {
	root := fstest.MapFS{
		"demo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
concept item {
  id  string  @required
}`)},
	}
	svc, err := memql.BuildOfflineSense(withLanguageLines(root))
	if err != nil {
		t.Fatalf("BuildOfflineSense: %v", err)
	}
	labels := func(src string) map[string]bool {
		out := map[string]bool{}
		for _, it := range svc.Complete(src, 1, len(src)+1, "p.memql") {
			out[it.Label] = true
		}
		return out
	}
	if got := labels("use demo."); !got["concepts"] {
		t.Errorf("`use demo.` should offer the concepts kind, got %v", got)
	}
	if got := labels("use demo.concepts.{ "); !got["item"] {
		t.Errorf("brace list should offer `item`, got %v", got)
	}
	// Namespace segment offers demo (and not a dump of concept names).
	if got := labels("use "); !got["demo"] || got["item"] {
		t.Errorf("`use ` should offer namespace demo only, got %v", got)
	}
}

// End-to-end over the REAL registry (embedded engine): an in-body `query <name>`
// invocation offers only query-kind functions (#2733) -- never mutations, logic,
// or tools. Property test so it does not pin specific engine function names.
func TestBuildOfflineSense_InvocationKindFilteredEndToEnd(t *testing.T) {
	svc, err := memql.BuildOfflineSense(nil)
	if err != nil {
		t.Fatalf("BuildOfflineSense over embedded tree: %v", err)
	}
	src := "logic probe {\n  query "
	items := svc.Complete(src, 2, len("  query ")+1, "x/logic.memql")
	if len(items) == 0 {
		t.Fatal("`query ` in a logic body offered nothing; expected the engine's query functions")
	}
	for _, it := range items {
		if it.Detail != "query" {
			t.Errorf("invocation completion offered a non-query item %q (kind %q); want only query-kind", it.Label, it.Detail)
		}
	}
}
