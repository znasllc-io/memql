package conformance

import (
	"os"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The current edition's corpus is written in the language this engine
// speaks: its manifest names parser.Edition and parser.LanguageVersion. An
// engine that moved its line without its corpus -- or the other way round --
// would be holding itself to cases written for another language.
func TestCorpusManifestMatchesTheEngine(t *testing.T) {
	vm := readCorpusManifest(t, os.DirFS("."), langparser.Edition)
	if vm.Language != langparser.LanguageVersion {
		t.Errorf("%s/manifest.json declares language %q; the engine speaks %q", langparser.Edition, vm.Language, langparser.LanguageVersion)
	}
	switch vm.Status {
	case "draft", "frozen":
	default:
		t.Errorf("%s/manifest.json status %q is neither draft nor frozen", langparser.Edition, vm.Status)
	}
}
