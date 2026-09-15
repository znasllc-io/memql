package memoryNodes

import (
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2613: absent @version means 1.0.0 on the LIVE unified-loader path --
// AssembleConceptIdFromDeclInDir derives the namespace from the directory
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

	// The directory-AWARE assembler throughout. @namespace was retired in
	// epic memql#5375, so AssembleConceptIdFromDecl (the directory-less form)
	// can no longer produce an id for any input -- the namespace comes from
	// the domain directory, which is what the "cognition" argument is. The
	// subject of this test is @version's 1.0.0 default (#2613), and that is
	// unchanged either way.
	absent := parse(`@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id, err := languageAst.AssembleConceptIdFromDeclInDir(absent, "cognition", "")
	if err != nil {
		t.Fatalf("assemble id without @version: %v", err)
	}
	if id != "v1:cognition:probeParticipant" {
		t.Errorf("absent @version: id = %q, want v1:cognition:probeParticipant", id)
	}

	explicit := parse(`@version("2.5.7")
@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id2, err := languageAst.AssembleConceptIdFromDeclInDir(explicit, "cognition", "")
	if err != nil {
		t.Fatalf("assemble id with explicit @version: %v", err)
	}
	if id2 != "v2:cognition:probeParticipant" {
		t.Errorf("explicit @version: id = %q, want v2:cognition:probeParticipant (explicit wins)", id2)
	}

	// The directory-LESS assembler now returns the empty id for EVERY input,
	// because @namespace was its only source (epic memql#5375). That is not
	// the old transitional skip wearing a new hat: it is the one caller of
	// this form -- the authoring sandbox's untitled-buffer path -- losing its
	// second way to place a concept, so a bundle with no origin path is now
	// refused rather than silently unplaced.
	bare := parse(`@description("probe")
concept probeParticipant {
  displayName string @required
}`)
	id3, err := languageAst.AssembleConceptIdFromDecl(bare)
	if err != nil || id3 != "" {
		t.Errorf("directory-less assembly: want the empty id, got %q err=%v", id3, err)
	}

	// The metadata sink defaults too. Concept.Version stores the
	// "v<major>" prefix form (concept_parser.go), so the default must
	// land as "v1" -- byte-identical to what an explicit
	// @version("1.0.0") produced before the #2613 strip.
	built, err := BuildConceptFromDecl(absent, "v1:cognition:probeParticipant")
	if err != nil {
		t.Fatalf("BuildConceptFromDecl without @version: %v", err)
	}
	if built.Version != "v1" {
		t.Errorf("built concept version = %q, want the v1 default", built.Version)
	}
	builtExplicit, err := BuildConceptFromDecl(explicit, "v2:cognition:probeParticipant")
	if err != nil {
		t.Fatalf("BuildConceptFromDecl with explicit @version: %v", err)
	}
	if builtExplicit.Version != "v2" {
		t.Errorf("built concept version = %q, want v2 (prefix form of the explicit 2.5.7)", builtExplicit.Version)
	}
}
