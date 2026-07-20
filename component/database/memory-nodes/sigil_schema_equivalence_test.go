package memoryNodes

import (
	"encoding/json"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2618: the sigil/enum-type spellings must build byte-identical
// concept schemas to their long forms -- the parser produces the same
// AST surface, and this pins the JSON-schema builder end of it.
func TestSigilAndEnumTypeSchemaEquivalence(t *testing.T) {
	build := func(src string) string {
		t.Helper()
		file, err := languageParser.ParseFile(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		decl := file.Definitions[0].(*languageAst.ConceptDecl)
		c, err := BuildConceptFromDecl(decl, "v1:probe:widget")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		raw, err := json.Marshal(c.Schemas)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// NOTE: the @enum ANNOTATION is args-block-only -- the concept
	// builder rejects it ("unknown property annotation @enum"), so on
	// concept fields the enum TYPE was always the only spelling. The
	// migration surface here is @required -> sigil alone.
	longForm := build(`@namespace("probe")
concept widget {
  label string @required
  status enum("open", "closed") @required
  note string
}`)
	shortForm := build(`@namespace("probe")
concept widget {
  label string!
  status enum("open", "closed")!
  note string
}`)
	if longForm != shortForm {
		t.Errorf("schemas diverge:\n long %s\nshort %s", longForm, shortForm)
	}
}
