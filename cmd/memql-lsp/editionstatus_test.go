package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The extension states its edition's STATUS, and this file holds that
// statement to the tree's own record of it.
//
// WHY THE EXTENSION NEEDS IT AT ALL. "MemQL: Show Language Reference" says
// which language it is showing -- the edition, the edition's status, and the
// grammar version -- because a grammar with no version on it is a grammar a
// reader cannot tell from yesterday's, and "frozen" is the difference between
// a form that is safe to build on and one that may be gone next month. The
// grammar and vocabulary come from the cluster, which states its edition and
// grammar version with them; NOTHING ON THE WIRE STATES THE STATUS. So the
// extension reads it from its own manifest, and this gate is what makes that
// reading true rather than a word somebody typed once.
//
// WHERE THE TRUTH LIVES. test/conformance/<edition>/manifest.json is the
// tree's record of an edition: its language line, its status, and the date it
// was frozen. test/conformance/corpus_manifest_test.go holds THAT file to the
// parser. This holds the extension's copy to that file, so the chain runs
// parser -> corpus manifest -> extension pin with no link resting on prose.
//
// WHY A COPY EXISTS AT ALL, rather than the extension reading the manifest:
// the manifest is a test fixture and ships in no VSIX. A packaged extension
// has its own package.json and nothing else, so the fact has to travel in the
// package.json -- which is where the edition and the grammar version already
// travel, for the same reason (editorparity_test.go).
//
// WHAT HAPPENS IF THIS IS IGNORED. The pin keeps its old value, the reference
// panel keeps printing it, and a reader is told an edition is frozen after it
// has been reopened -- the one fact on that page a reader cannot check against
// the artifacts below it.

// checkedInCorpusManifest is the edition record this gate reads, relative to
// cmd/memql-lsp. The edition directory is named by the edition itself, so the
// path is composed from parser.Edition rather than written out: an edition
// bump then fails here by naming a file that does not exist, which is the
// right failure -- a new edition needs a new corpus directory before anything
// can state its status.
func checkedInCorpusManifest(edition string) string {
	return fmt.Sprintf("../../test/conformance/%s/manifest.json", edition)
}

// corpusManifest is the edition record, read for the one field this gate
// judges. Its own type rather than the conformance package's: that package is
// a test package in another module, and importing it here to read one string
// would couple the editor gate to the whole corpus harness.
type corpusManifest struct {
	Edition string `json:"edition"`
	Status  string `json:"status"`
}

// extensionStatusPin is the manifest's `memql` block, read for the status
// alone. Separate from editorparity_test.go's extensionLanguageManifest for
// the reason that type's own comment gives: it is built literally by that
// file's fixtures, and widening it would break them for a field only this
// gate reads.
type extensionStatusPin struct {
	MemQL *struct {
		Edition string `json:"edition"`
		Status  string `json:"status"`
	} `json:"memql"`
}

// TestExtensionPinsTheEditionStatus refuses a pin that disagrees with the
// edition record, in either direction, and refuses a missing one.
func TestExtensionPinsTheEditionStatus(t *testing.T) {
	var record corpusManifest
	path := checkedInCorpusManifest(parser.Edition)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("parser.Edition is %q but %s does not exist: an edition states its status in its corpus manifest, and the extension's pin is read from it (%v)",
			parser.Edition, path, err)
	}
	loadExtensionJSON(t, path, &record)

	if record.Status == "" {
		t.Fatalf("%s states no status; test/conformance/corpus_manifest_test.go admits only \"frozen\" for the edition this engine writes", path)
	}
	if record.Edition != parser.Edition {
		t.Fatalf("%s declares edition %q; the directory is named for edition %q", path, record.Edition, parser.Edition)
	}

	var pin extensionStatusPin
	loadExtensionJSON(t, checkedInExtensionManifest, &pin)
	switch {
	case pin.MemQL == nil:
		t.Fatalf("editors/vscode/package.json has no \"memql\" block; add \"memql\": {\"edition\": %q, \"status\": %q, \"grammarVersion\": ...}",
			parser.Edition, record.Status)
	case pin.MemQL.Status == "":
		t.Errorf("editors/vscode/package.json pins no \"status\", so \"MemQL: Show Language Reference\" cannot say whether edition %s is frozen -- and an unstated status and a draft edition look identical as an absence. Set \"memql\": {\"status\": %q}.",
			parser.Edition, record.Status)
	case pin.MemQL.Status != record.Status:
		t.Errorf("editors/vscode/package.json pins \"status\": %q, but %s records %q. The extension prints its pin, so this is a reader being told an edition is %s when it is %s. Set \"memql\": {\"status\": %q}.",
			pin.MemQL.Status, path, record.Status, pin.MemQL.Status, record.Status, record.Status)
	}
	// The status belongs to the edition it is pinned beside. A pin naming one
	// edition and the status of another is worse than no status at all,
	// because the panel prints them on adjacent rows as one fact.
	if pin.MemQL != nil && pin.MemQL.Edition != record.Edition {
		t.Errorf("editors/vscode/package.json pins edition %q beside status %q, but %q is the status of edition %q",
			pin.MemQL.Edition, pin.MemQL.Status, record.Status, record.Edition)
	}
}
