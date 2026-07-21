package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestResolveSymbol_CrossFileConcept locks the canonical case:
// `alias.name` resolves to a concept in the imported file, complete
// with assembled concept ID.
func TestResolveSymbol_CrossFileConcept(t *testing.T) {
	root := fstest.MapFS{
		"cognition/participant.memql": {Data: []byte(`@version("1.0.0")
@namespace("cognition")
@description("a participant")
concept participant {
  partitionId string
}`)},
		"queries/foo.memql": {Data: []byte(`import (
	"../cognition/participant" as cog
)
@description("foo")
func (Query) foo(_ any) (any, error) { return nil, nil }`)},
	}

	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := tree.ResolveSymbol("queries/foo.memql", "cog.participant")
	if err != nil {
		t.Fatalf("ResolveSymbol: %v", err)
	}
	if res.Kind != SymbolConcept {
		t.Errorf("Kind = %v, want SymbolConcept", res.Kind)
	}
	if res.File != "cognition/participant.memql" {
		t.Errorf("File = %q, want cognition/participant.memql", res.File)
	}
	if res.Name != "participant" {
		t.Errorf("Name = %q, want participant", res.Name)
	}
	if res.ConceptId != "v1:cognition:participant" {
		t.Errorf("ConceptId = %q, want v1:cognition:participant", res.ConceptId)
	}
}

// TestResolveSymbol_CrossFileQuery locks the query resolution path.
func TestResolveSymbol_CrossFileQuery(t *testing.T) {
	root := fstest.MapFS{
		"queries/users.memql": {Data: []byte(`@description("users query")
func (Query) listUsers(_ any) (any, error) { return nil, nil }`)},
		// The tool file only needs the import + a parseable body so
		// ResolveSymbol has somewhere to anchor the `q` alias. Prior
		// to #293 this file carried `func (Tool) userTool() (any,
		// error)` with a missing `_ any` parameter -- the parser
		// rejected it but dslimports.Load's pre-#293 fallback
		// silently swallowed the parse error, so the test still
		// worked end-to-end. With the fallback now narrowed to
		// ErrEmptyInput only, the fixture has to be syntactically
		// clean (or stripped to comments + imports).
		"tools/userTool.memql": {Data: []byte(`import (
	"../queries/users" as q
)
// userTool body intentionally omitted; the test only exercises
// the import-alias resolution path, not the tool declaration.`)},
	}

	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := tree.ResolveSymbol("tools/userTool.memql", "q.listUsers")
	if err != nil {
		t.Fatalf("ResolveSymbol: %v", err)
	}
	if res.Kind != SymbolQuery {
		t.Errorf("Kind = %v, want SymbolQuery", res.Kind)
	}
	if res.Name != "listUsers" {
		t.Errorf("Name = %q, want listUsers", res.Name)
	}
}

// TestResolveSymbol_LocalReference locks resolution against the
// caller's own top-level declarations.
func TestResolveSymbol_LocalReference(t *testing.T) {
	root := fstest.MapFS{
		"foo.memql": {Data: []byte(`@description("self")
func (Query) selfQuery(_ any) (any, error) { return nil, nil }`)},
	}

	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := tree.ResolveSymbol("foo.memql", "selfQuery")
	if err != nil {
		t.Fatalf("ResolveSymbol: %v", err)
	}
	if res.Kind != SymbolQuery {
		t.Errorf("Kind = %v, want SymbolQuery", res.Kind)
	}
}

// TestResolveSymbol_UnknownAlias locks the alias-not-imported error.
func TestResolveSymbol_UnknownAlias(t *testing.T) {
	root := fstest.MapFS{
		"a.memql": {Data: []byte(`@description("a")
func (Query) a(_ any) (any, error) { return nil, nil }`)},
		"b.memql": {Data: []byte(`@description("b")
func (Query) b(_ any) (any, error) { return nil, nil }`)},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// b never imported a; lookup should fail.
	_, err = tree.ResolveSymbol("b.memql", "missing.foo")
	if err == nil {
		t.Fatal("expected error for unknown alias, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention the missing alias", err.Error())
	}
}

// TestResolveSymbol_UnknownName locks the "name not declared in
// target file" error.
func TestResolveSymbol_UnknownName(t *testing.T) {
	root := fstest.MapFS{
		"target.memql": {Data: []byte(`@description("target")
func (Query) realName(_ any) (any, error) { return nil, nil }`)},
		"caller.memql": {Data: []byte(`import (
	"./target" as t
)
@description("caller")
func (Query) caller(_ any) (any, error) { return nil, nil }`)},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = tree.ResolveSymbol("caller.memql", "t.bogusName")
	if err == nil {
		t.Fatal("expected error for unknown name, got nil")
	}
}

// TestResolveSymbol_ConceptWithoutVersion locks the transitional
// case: a concept that hasn't been migrated to @version + @namespace
// resolves with ConceptId == "" but no error.
func TestResolveSymbol_ConceptWithoutVersion(t *testing.T) {
	root := fstest.MapFS{
		"legacy/x.memql": {Data: []byte(`@description("legacy concept")
concept oldStyle { name string }`)},
		"caller.memql": {Data: []byte(`import (
	"./legacy/x" as x
)
@description("c")
func (Query) c(_ any) (any, error) { return nil, nil }`)},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := tree.ResolveSymbol("caller.memql", "x.oldStyle")
	if err != nil {
		t.Fatalf("ResolveSymbol: %v", err)
	}
	if res.Kind != SymbolConcept {
		t.Errorf("Kind = %v, want SymbolConcept", res.Kind)
	}
	if res.ConceptId != "v1:legacy:oldStyle" {
		t.Errorf("ConceptId = %q, want the directory-derived id (#2614: absent @namespace derives the domain directory; the transitional empty-id skip is over)", res.ConceptId)
	}
}

// TestResolveSymbol_BadShape locks the format-checks on `ref`.
func TestResolveSymbol_BadShape(t *testing.T) {
	root := fstest.MapFS{
		"a.memql": {Data: []byte(`@description("a")
func (Query) a(_ any) (any, error) { return nil, nil }`)},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, ref := range []string{".bad", "bad.", "."} {
		_, err := tree.ResolveSymbol("a.memql", ref)
		if err == nil {
			t.Errorf("expected error for ref=%q, got nil", ref)
		}
	}
}
