package memoryNodes

import (
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2613: absent @version means 1.0.0 on the LIVE unified-loader path --
// AssembleConceptIdFromDecl selects the annotated assembly on @namespace
// alone and defaults the version. Before the default, a version-less
// concept fell to the "legacy loader handles it" skip (a path the flat
// tree does not have) and silently never registered: the codemod strip
// broke concept registration tree-wide until this landed.
func TestConceptVersionDefault_UnifiedAssembly(t *testing.T) {
	parse := func(src string) *languageAst.ConceptDecl {
		t.Helper()
		file, err := languageParser.ParseFile(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		for _, def := range file.Definitions {
			if c, ok := def.(*languageAst.ConceptDecl); ok {
				return c
			}
		}
		t.Fatal("no concept decl parsed")
		return nil
	}

	absent := parse(`@namespace("cognition")
@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id, err := languageAst.AssembleConceptIdFromDecl(absent)
	if err != nil {
		t.Fatalf("assemble id without @version: %v", err)
	}
	if id != "v1:cognition:probeParticipant" {
		t.Errorf("absent @version: id = %q, want v1:cognition:probeParticipant", id)
	}

	explicit := parse(`@version("2.5.7")
@namespace("cognition")
@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id2, err := languageAst.AssembleConceptIdFromDecl(explicit)
	if err != nil {
		t.Fatalf("assemble id with explicit @version: %v", err)
	}
	if id2 != "v2:cognition:probeParticipant" {
		t.Errorf("explicit @version: id = %q, want v2:cognition:probeParticipant (explicit wins)", id2)
	}

	// No @namespace: still the transitional silent skip, unchanged.
	bare := parse(`@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id3, err := languageAst.AssembleConceptIdFromDecl(bare)
	if err != nil || id3 != "" {
		t.Errorf("no @namespace: want the transitional empty id, got %q err=%v", id3, err)
	}
}
