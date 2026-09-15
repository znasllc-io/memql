package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tliron/commonlog"
	protocol "github.com/tliron/glsp/protocol_3_16"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// TestCompletion_ConceptImportIsAnAdditionalEdit: at the concept slot, the
// suggestion that imports an unimported concept inserts the concept's NAME
// at the cursor and carries the file-top `use` line as an additionalTextEdit
// (memql#5359) -- the client applies both. It used to insert the whole
// `use` line at the slot (`query use gadgets.concepts.{ gadget }`), which
// does not parse.
func TestCompletion_ConceptImportIsAnAdditionalEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "gadgets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The domain declares its language line, as a real bundle domain must
	// (memql#5357); without it the engine refuses the domain's concepts.
	line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}
	if err := os.WriteFile(filepath.Join(dir, "gadgets", dslfs.ManifestFile), []byte(line.Render()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gadgets", "concepts.memql"), []byte(`@version("1.0.0")
@description("A gadget.")
concept gadget {
  label string @required @description("Label")
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	commonlog.Configure(-4, nil)
	s := newServer(dir, commonlog.GetLogger(lsName))
	s.buildSense(nil)

	for _, tc := range []struct {
		doc  string
		line int
		want protocol.TextEdit
	}{
		// No imports yet: the `use` line goes at the top, with a blank line.
		{"query ", 0, protocol.TextEdit{
			Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
			NewText: "use gadgets.concepts.{ gadget }\n\n",
		}},
		// After the file's last `use`.
		{"use cluster.concepts.{ node }\nquery ", 1, protocol.TextEdit{
			Range:   protocol.Range{Start: protocol.Position{Line: 1, Character: 0}, End: protocol.Position{Line: 1, Character: 0}},
			NewText: "use gadgets.concepts.{ gadget }\n",
		}},
	} {
		const uri = "file:///b.memql"
		s.docs.open(uri, tc.doc)
		var item *protocol.CompletionItem
		for _, it := range completionAt(t, s, uri, tc.line, 6) {
			if it.Label == "use gadgets.concepts.{ gadget }" {
				it := it
				item = &it
			}
		}
		if item == nil {
			t.Fatalf("%q: no import suggestion for the unimported gadget concept", tc.doc)
		}
		if item.InsertText == nil || *item.InsertText != "gadget" {
			t.Errorf("%q: the import suggestion inserts %v at the concept slot, want the name alone", tc.doc, item.InsertText)
		}
		if len(item.AdditionalTextEdits) != 1 || item.AdditionalTextEdits[0] != tc.want {
			t.Errorf("%q: additionalTextEdits = %+v, want [%+v]", tc.doc, item.AdditionalTextEdits, tc.want)
		}
	}
}
