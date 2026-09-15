package main

import (
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The tutorial, downloadable source and declared edition must stay one usable
// example. The site generates its tabs/download from this same source; the
// existing snippet gate validates the tutorial with the real parser.
func TestReadingListTutorialMatchesSource(t *testing.T) {
	source, err := os.ReadFile("examples/reading-list/reading.memql")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("docs/public/language/first-program.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := extractMemqlBlocks(string(guide))
	if len(blocks) != 1 || blocks[0].marker != "" || strings.TrimSpace(blocks[0].body) != strings.TrimSpace(string(source)) {
		t.Fatal("the first-program tutorial must contain the complete reading-list source as one validated MemQL block")
	}
	manifest, err := os.ReadFile("examples/reading-list/memql.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := parser.CheckLanguageLineFile("reading-list", "examples/reading-list/memql.toml", manifest); len(problems) != 0 {
		t.Fatalf("example's language manifest is not supported: %v", problems)
	}
	if diagnostic := validateSnippet("examples/reading-list"); diagnostic != "" {
		t.Fatal(diagnostic)
	}
}
