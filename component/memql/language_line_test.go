package memql

import (
	"errors"
	"testing"
	"testing/fstest"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// language_line_test.go -- every tree declares the language it is written in,
// and the loader refuses one that declares none or a newer one (memql#5357;
// D4 of docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).

// languageLineFile is a memql.toml declaring the engine's own line. Every
// throwaway domain a test mounts carries one, exactly as a real bundle domain
// must (memql#5357); without it the domain's concepts are not built and Init
// refuses the tree.
func languageLineFile() *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(currentLanguageLine())}
}

// currentLanguageLine is the text of that file, for fixtures written to disk.
func currentLanguageLine() string {
	return dslfs.Manifest{Language: langparser.LanguageVersion, Edition: langparser.Edition}.Render()
}

// withLanguageLine adds the engine's own line to a tree rooted at a domain's
// contents -- the shape RegisterTree takes -- and returns it.
func withLanguageLine(tree fstest.MapFS) fstest.MapFS {
	tree[dslfs.ManifestFile] = languageLineFile()
	return tree
}

// withLanguageLines adds the engine's own line to every domain of a tree
// rooted ABOVE its domains -- the shape a lint root or a MEMQL_DSL_PATH root
// takes -- leaving a domain that already declares one alone, and returns it.
func withLanguageLines(root fstest.MapFS) fstest.MapFS {
	domains := map[string]bool{}
	for p := range root {
		if d := langparser.LanguageLineDomainOf(p); d != "" {
			domains[d] = true
		}
	}
	for d := range domains {
		if _, declared := root[d+"/"+dslfs.ManifestFile]; !declared {
			root[d+"/"+dslfs.ManifestFile] = languageLineFile()
		}
	}
	return root
}

// languageLineTrait is a construct that loads clean on its own, so the only
// problem a fixture carrying it can have is its language line.
const languageLineTrait = `trait langLineProbeTrait = row => row.active == true
`

// A boot refused at the concept phase leads with what it found, and a domain
// whose line is refused is not a "malformed concept" -- app/database.go heads
// its refusal with this, so an operator reads the right thing to fix.
func TestLanguageLine_ConceptSkipsHeadingNamesWhatWasFound(t *testing.T) {
	refusal := func(domain, code string) ConceptSkip {
		s := languageLineSkip(LanguageLineProblem{Domain: domain, Source: domain + "/memql.toml", Code: code, Message: "m [" + code + "]"})
		return ConceptSkip{File: s.File, Err: errors.New(s.Err), Refusal: &s}
	}
	malformedSkip := ConceptSkip{File: "shop/concepts.memql", Concept: "v1:shop:order", Err: errors.New("bad type")}
	for _, tc := range []struct {
		name  string
		skips []ConceptSkip
		want  string
	}{
		{"refusals only, one domain wrong twice", []ConceptSkip{refusal("shop", "language_version_newer"), refusal("shop", "edition_unknown"), refusal("orders", "language_line_missing")},
			"2 domain(s) whose language line this engine will not read"},
		{"malformed only", []ConceptSkip{malformedSkip}, "1 malformed concept(s)"},
		{"both", []ConceptSkip{refusal("shop", "language_line_missing"), malformedSkip, malformedSkip},
			"1 domain(s) whose language line this engine will not read, and 2 malformed concept(s)"},
	} {
		if got := ConceptSkipsHeading(tc.skips); got != tc.want {
			t.Errorf("%s: ConceptSkipsHeading = %q, want %q", tc.name, got, tc.want)
		}
	}
}
