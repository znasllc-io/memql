package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

// deprecation_window_docs_test.go -- memql#5390.
//
// docs/public/language/memql.md publishes the forms currently in a deprecation
// window, with the release each was deprecated in and the release it stops
// loading at. Those two dates are a PROMISE, and a page that promises a date
// the engine does not keep is worse than no page: an author reads "0.25",
// plans around it, and the form goes at some other release or never.
//
// So the table is held to the registry rather than maintained beside it. The
// registry is a leaf in another module and has no business rendering Markdown,
// and the page is prose the table sits inside, so the renderer lives here and
// the failure prints the block to paste. Either half moving alone fails: a form
// added to the registry and not to the page, a page edited by hand, a window
// whose arithmetic moved.

const (
	deprecationTableBegin = "<!-- deprecation-window:begin"
	deprecationTableEnd   = "<!-- deprecation-window:end -->"
	memqlLanguagePage     = "docs/public/language/memql.md"
)

// renderDeprecationTable is the table the registry says the page should carry.
func renderDeprecationTable() string {
	var b strings.Builder
	b.WriteString("| Form | Write instead | Rewrite | Deprecated in | Stops loading in | Rule |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, f := range deprecation.Forms() {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s | `%s` |\n",
			f.Spelling, f.Replacement, f.Migrator, f.DeprecatedIn, f.RefusedFrom(), f.Rule)
	}
	return b.String()
}

func TestDeprecationWindowTableMatchesTheRegistry(t *testing.T) {
	raw, err := os.ReadFile(memqlLanguagePage)
	if err != nil {
		t.Fatalf("%s is missing: %v", memqlLanguagePage, err)
	}
	page := string(raw)

	_, after, ok := strings.Cut(page, deprecationTableBegin)
	if !ok {
		t.Fatalf("%s carries no %s marker; the deprecation-window table is what the page promises "+
			"about every form in a window, and removing the marker removes the gate with it",
			memqlLanguagePage, deprecationTableBegin)
	}
	// The begin marker carries a trailing comment; the block starts after it.
	_, after, ok = strings.Cut(after, "-->")
	if !ok {
		t.Fatalf("the %s marker in %s is not closed", deprecationTableBegin, memqlLanguagePage)
	}
	block, _, ok := strings.Cut(after, deprecationTableEnd)
	if !ok {
		t.Fatalf("%s carries no %s marker", memqlLanguagePage, deprecationTableEnd)
	}

	got, want := strings.TrimSpace(block), strings.TrimSpace(renderDeprecationTable())
	if got == want {
		return
	}
	t.Fatalf("the deprecation-window table in %s is not what component/language/deprecation holds.\n\n"+
		"got:\n%s\n\nwant:\n%s\n\nPaste the wanted block between the markers. If a form's dates changed, "+
		"check that was intended: the two releases are a promise, and the second is DERIVED "+
		"(DeprecatedIn + %d minor releases), so it moves only when DeprecatedIn does.",
		memqlLanguagePage, got, want, deprecation.MinimumMinorReleases)
}

// Every form the page lists names a window the engine will actually keep: the
// release it stops loading at refuses, and the release before it does not. The
// table test above pins the strings; this pins what they MEAN, so a table that
// agrees with a renderer that agrees with nothing still fails.
func TestThePublishedWindowIsTheWindowTheEngineKeeps(t *testing.T) {
	for _, f := range deprecation.Forms() {
		if f.RefusesAt(f.DeprecatedIn) {
			t.Errorf("form %s refuses at %s, the release that deprecated it", f.Rule, f.DeprecatedIn)
		}
		if !f.RefusesAt(f.RefusedFrom()) {
			t.Errorf("form %s does not refuse at %s, the release the page says it stops loading at",
				f.Rule, f.RefusedFrom())
		}
		if !strings.Contains(f.Warning(), f.RefusedFrom()) {
			t.Errorf("form %s: the warning does not name %s, the release the page publishes:\n%s",
				f.Rule, f.RefusedFrom(), f.Warning())
		}
	}
}
