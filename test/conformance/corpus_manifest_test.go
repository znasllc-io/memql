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
	// The edition this engine WRITES is frozen (memql#5390). Before the freeze
	// this accepted "draft" or "frozen" and so read nothing: a status field no
	// test enforces is a comment with quotes around it, and the point of
	// flipping it is that something notices if it flips back.
	//
	// Scoped to parser.Edition deliberately. A LATER edition under development
	// lives in its own directory and is legitimately "draft"; only the one this
	// engine writes has to be frozen, which is what "edition 2026 is frozen"
	// actually claims.
	if vm.Status != "frozen" {
		t.Errorf("%s/manifest.json status is %q; the edition this engine writes is FROZEN.\n"+
			"If you are drafting a later edition, give it its own directory. This one is the language "+
			"clusters and bundles are already written against: a form it holds may be ADDED to freely, "+
			"but removed only through the deprecation window (component/language/deprecation) and a "+
			"reservation in component/language/reserved.json, which cmd/memqlbreaking enforces.",
			langparser.Edition, vm.Status)
	}
}

// The status a cluster REPORTS for its edition -- the languageStatus builtin,
// and MemQL OS's Settings -> Language, which prints the frozen guarantee only
// under a frozen edition -- is parser.EditionStatus, a constant, because the
// engine never reads its test tree at run time.
//
// The corpus manifest is where the status is DECIDED, so the two move in one
// change and this holds them equal in BOTH directions: a cluster can never
// call a draft edition frozen, and never call a frozen one draft. The first
// would promise operators a stability the language is not offering; the second
// would tell them to migrate away from forms nothing is going to refuse.
func TestEditionStatusMatchesTheCorpusManifest(t *testing.T) {
	vm := readCorpusManifest(t, os.DirFS("."), langparser.Edition)
	if langparser.EditionStatus != vm.Status {
		t.Errorf("parser.EditionStatus is %q and %s/manifest.json says %q; flip both in the same change",
			langparser.EditionStatus, langparser.Edition, vm.Status)
	}
}
