package memql

// memql#2909: a concept property typed with a spelling the schema builder does
// not know (`boolean` for `bool`) makes BuildConceptFromDecl fail, and the
// unified loader responds by WARNING and dropping the whole concept. Not the
// property -- the concept, and with it every query, mutation and shape bound
// to it.
//
// The warning goes to a logger. LintUnifiedTree collects eng.loadReport.Skipped,
// which covers construct-phase skips, so a concept that never reached the
// registry leaves no diagnostic behind. memqllint is the only pre-boot gate a
// product bundle has, so the bundle lints clean and loses a concept at boot.

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestLintParity_ConceptWithUnknownPropertyType is the witness. `boolean` is
// accepted for the same author-facing value everywhere else in the tree --
// args binding (component/automations/args_binding.go), value type-checking
// (component/memql/parser.go) -- so it is a predictable thing to type, and the
// concept schema builder is the outlier that rejects it.
func TestLintParity_ConceptWithUnknownPropertyType(t *testing.T) {
	root := fstest.MapFS{
		"lintbadtype/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintbadtype")
@description("A widget with a mistyped property type.")
concept widget {
  label    string   @required  @description("Widget label.")
  enabled  boolean  @description("Whether the widget is enabled.")
}
`)},
	}
	diags := lint(t, root)
	if len(diags) == 0 {
		t.Fatal("a concept whose schema fails to build must produce a diagnostic: the unified " +
			"loader drops the WHOLE concept, and memqllint is the only pre-boot gate a product " +
			"bundle has (memql#2909)")
	}
	if !diagsContain(diags, "widget") {
		t.Errorf("the diagnostic must name the dropped concept, or an author cannot find it.\n  got: %+v", diags)
	}
	if !diagsContain(diags, "enabled") {
		t.Errorf("the diagnostic must name the offending property.\n  got: %+v", diags)
	}
}

// TestLintParity_UnknownPropertyTypeNamesItsFile pins the attribution. A
// bundle can hold hundreds of concepts; a diagnostic that cannot say which
// file to open costs more than it saves.
func TestLintParity_UnknownPropertyTypeNamesItsFile(t *testing.T) {
	root := fstest.MapFS{
		"lintbadfile/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintbadfile")
@description("A gadget with a mistyped property type.")
concept gadget {
  label  string     @required  @description("Gadget label.")
  size   quantities @description("Bogus type.")
}
`)},
	}
	diags, _, err := LintUnifiedTree(nil, root)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "gadget") && strings.Contains(d.File, "lintbadfile/concepts.memql") {
			found = true
		}
	}
	if !found {
		t.Errorf("the diagnostic must carry the origin file of the dropped concept (memql#2909).\n  got: %+v", diags)
	}
}
