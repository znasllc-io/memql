package conformance

// The docs cannot show a form the parser refuses (memql#5388, D24).
//
// Every fenced ```memql block under docs/public/language is extracted and
// handed to the parser -- the SAME entry point the conformance corpus reads a
// case with, the edition's front end followed by the parse every loader runs.
// A block that does not parse fails this test, naming the file and the line
// its fence opens on.
//
// # Why this and not "the examples are drawn from the corpus"
//
// Drawing every example from a corpus file would mean either rewriting every
// page as a transclusion or copying files around, and both make the pages
// worse to read. The PROPERTY the record asks for is that an example which
// stops loading fails the build, and parsing every block delivers exactly
// that, over every page, including the ones nobody remembered to wire up.
//
// HALF OF THAT WAS WRONG, and docs_corpus_examples_test.go is the correction:
// PARSING IS NOT LOADING. A block that parses can still name a construct that
// does not exist, project a field its concept does not declare, read the actor
// without declaring `@actor`, or reach across a namespace with no `use` import
// -- the engine refuses every one of those, and this gate sees none of them.
// Measured on 2026-09-20 by lifting functions.md's examples into the corpus
// unchanged: 16 of 30 were refused at load while this gate stayed green.
//
// The correction keeps the page readable rather than transcluding it: a fence
// on a covered page names its conformance case in a `<!-- corpus: ... -->`
// marker and is held to it. What survives here unchanged is the second half of
// the argument above -- this gate reads EVERY page under docs/public/language,
// including any page added tomorrow that the corpus gate's coverage list does
// not yet name. Nothing here may be narrowed on the grounds that the other
// gate exists; the two overlap deliberately, and this one is the wider,
// weaker of the two.
//
// # The fence markers, and why there is no fourth
//
// The repo already had a marker convention for this, introduced with
// TestDocsMemqlSnippets (memql#4091) in the root package, and this gate uses
// it rather than inventing a second spelling of the same idea:
//
//	```memql            validated: it must parse
//	```memql fragment   incomplete by design -- SKIPPED, and counted
//	```memql retired    a deliberate don't-do-this example
//
// What CHANGES here is the third one. TestDocsMemqlSnippets skips a `retired`
// block, which means the marker can hide a broken example: write anything at
// all under it and nothing looks. This gate requires a `retired` block to
// actually FAIL to parse. That is what makes the marker a claim rather than an
// exemption -- a page that says "this form is refused" is now checked against
// a parser that refuses it.
//
// # The two gates are deliberately different, and the difference is the point
//
// TestDocsMemqlSnippets loads a block through dslimports.Load and checks its
// local references resolve. This one runs the EDITION FRONT END and the
// loaders' parse, which is where every retired spelling of edition 2026 is
// refused. A block can pass one and fail the other, and each failure means
// something different: an unresolved reference, or a form the language no
// longer has.
//
// A `retired` block is held to BOTH, because the language refuses at both
// stages and which stage a given form is refused at is not something a page
// author should have to know. `sort(folders, "name", "asc")` parses cleanly
// and is refused when the name is resolved; a detached annotation preamble
// parses cleanly and is refused by VerifyPreambleAttachment. Requiring only a
// PARSE refusal would have told the author to re-mark a page that is telling
// the truth.
//
// # What this gate CANNOT see, measured rather than assumed
//
// The language refuses at three stages: the edition front end plus the parse
// (here), the offline lint (here, for a `retired` block), and ENGINE LOAD --
// the concept translator, the function registry, the automation loader. The
// third is out of reach without booting an engine per block, and the gap is
// real rather than theoretical: a concept field carrying the retired
// `@default` parses clean, lints clean, and is refused only by the concept
// translator. Measured on 2026-09-20 by adding exactly that block to a page
// and watching BOTH docs gates stay green.
//
// So a page showing a load-refused form marks it `retired` and names the
// refusal's rule code in the prose above it; the corpus is what holds that
// claim, and refusalCodeAbove plus corpusPinsRefusal is what checks the corpus
// really does.
//
// # Fragments
//
// A doc example is often a FRAGMENT -- a bare `filter` line, an `args` block,
// a call written to show its shape. A fragment is skipped, and the skip is
// COUNTED AND PRINTED: a page of nothing but skipped fragments must not read
// as a covered page, and a count that quietly grows is how a gate stops
// gating.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/core/repowalk"
)

// docsLanguageDir is the tree this gate covers, relative to this package.
const docsLanguageDir = "../../docs/public/language"

// docFence matches a fenced block whose info string's first token is `memql`,
// capturing the marker that follows it and the block's body.
var docFence = regexp.MustCompile("(?s)```memql([a-z- ]*)\n(.*?)```")

func TestDocsExamplesParse(t *testing.T) {
	entries, err := os.ReadDir(docsLanguageDir)
	if err != nil {
		t.Fatalf("read %s: %v", docsLanguageDir, err)
	}
	var total, parsed, refused, pinned, fragments int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(docsLanguageDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		page := string(raw)
		for _, loc := range docFence.FindAllStringSubmatchIndex(page, -1) {
			marker := strings.TrimSpace(page[loc[2]:loc[3]])
			src := page[loc[4]:loc[5]]
			if strings.TrimSpace(src) == "" {
				continue
			}
			total++
			line := 1 + strings.Count(page[:loc[0]], "\n")
			where := fenceLocation(e.Name(), line)

			if marker == "fragment" {
				fragments++
				continue
			}
			kind, err := parseDocExample(src)
			switch {
			case kind == docExampleFragment:
				// A bare block the helper cannot place is a fragment in fact
				// if not in marking. It is named, so the page can be fixed by
				// marking it -- a silent skip is how the count stops meaning
				// anything.
				fragments++
				t.Logf("SKIPPED (fragment): %s -- %s", where, firstLine(src))
			case marker == "retired" && err == nil && docExampleLoads(t, src):
				// The language refuses at three stages, and this gate reaches
				// two. A form refused at ENGINE load -- an unknown call in a
				// statement body, say -- parses and lints clean here, so the
				// page carries the refusal's RULE CODE in the prose above the
				// block and the CORPUS is what holds the claim.
				if code := refusalCodeAbove(page, loc[0]); code != "" && corpusPinsRefusal(t, code) {
					pinned++
					t.Logf("PINNED BY THE CORPUS: %s -- refused at engine load as [%s]", where, code)
					continue
				}
				t.Errorf("%s: a ```memql retired block PARSES AND LOADS, and no corpus case is pinned for it.\n\n"+
					"Either the form came back and the marker is stale, or it is refused LATER than this gate "+
					"reaches -- at engine load rather than at parse or lint. For the second, name the refusal's "+
					"rule code in the prose above the block, in the [rule_code] form the refusals print it in, "+
					"and add the case to test/conformance/2026/ so the claim is held by something:\n%s", where, src)
			case marker == "retired":
				refused++
			case err != nil:
				t.Errorf("%s: a ```memql example does not parse: %v\n%s", where, err, src)
			default:
				parsed++
			}
		}
	}
	if total == 0 {
		t.Fatal("no ```memql blocks were found in docs/public/language at all: this gate would pass over " +
			"a docs tree with every example broken, which is the fail-open shape it exists to prevent")
	}
	if parsed == 0 {
		t.Fatal("no ```memql block PARSED: a gate whose positive half never fires proves the parser is " +
			"reachable from here and nothing else")
	}
	if refused == 0 {
		t.Fatal("no ```memql retired block was refused: either the pages stopped showing a retired form, or " +
			"the marker stopped being read. Both make the third of this gate's three answers vacuous")
	}
	t.Logf("docs examples in %s: %d blocks -- %d parsed, %d refused as retired, %d pinned by the corpus, %d skipped as fragments",
		docsLanguageDir, total, parsed, refused, pinned, fragments)
}

// docExampleLoads reports whether a block survives the LOAD pass as well as
// the parse: the import graph, referential integrity, symbol resolution and
// preamble attachment, which is the same pipeline cmd/memqllint runs on one
// file and the root package's TestDocsMemqlSnippets runs on a bare block.
//
// It is asked only of a `retired` block, and only after the parse accepted it,
// so the ordinary path pays nothing for it.
func docExampleLoads(t *testing.T, src string) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snippet.memql"), []byte(src), 0o644); err != nil {
		t.Fatalf("write temp snippet: %v", err)
	}
	tree, err := dslimports.Load(os.DirFS(dir))
	if err != nil {
		var le *dslimports.LoadError
		if errors.As(err, &le) && len(le.Diagnostics) > 0 {
			return false
		}
		return false
	}
	if tree == nil {
		return false
	}
	if len(tree.VerifyReferentialIntegrity()) > 0 ||
		len(tree.VerifyAllSymbolReferences()) > 0 ||
		len(tree.VerifyPreambleAttachment()) > 0 {
		return false
	}
	return true
}

// refusalRuleCode matches the `[rule_code]` a refusal message ends with, which
// is how the pages quote one.
var refusalRuleCode = regexp.MustCompile(`\[([a-z][a-z0-9_]{4,})\]`)

// refusalCodeAbove returns the last rule code quoted in the prose immediately
// above a fence, or "". The window is deliberately short: a code named four
// sections earlier is not this block's claim.
func refusalCodeAbove(page string, fenceAt int) string {
	const window = 1200
	from := fenceAt - window
	if from < 0 {
		from = 0
	}
	matches := refusalRuleCode.FindAllStringSubmatch(page[from:fenceAt], -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// corpusPinsRefusal reports whether the conformance corpus holds a case whose
// expected refusal carries this rule code. It reads the corpus's expect.json
// files as text rather than decoding them: the question is "is this code
// pinned anywhere", and a decoder here would be a second reader of a format
// corpus_test.go already owns.
func corpusPinsRefusal(t *testing.T, code string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "expect.json" || found {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), `"`+code+`"`) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the corpus for %q: %v", code, err)
	}
	return found
}

// docExampleKind is what parseDocExample decided a block was.
type docExampleKind int

const (
	// docExampleFile is a block that opens on a construct keyword: it is read
	// as a whole .memql file.
	docExampleFile docExampleKind = iota
	// docExampleLambda is a block that is one lambda (`row => ...`).
	docExampleLambda
	// docExampleFragment is a block that is neither, and is skipped.
	docExampleFragment
)

// parseDocExample parses one doc example through the entry point its shape
// calls for, and says which one it chose so the caller can count a skip.
//
// The wrapping rule is deliberately small, because a clever one hides what it
// did: a block that OPENS on a construct keyword is a file; a block that is a
// lambda is a lambda; anything else is a fragment. Leading comments, doc
// comments, annotations and `use` imports are skipped over when looking for
// the opening word -- they are what a real construct is preceded by.
func parseDocExample(src string) (docExampleKind, error) {
	head := firstSignificantWord(src)
	switch {
	case head == "":
		return docExampleFragment, nil
	case isConstructKeyword(head):
		_, err := corpusParseFile(langparser.Edition, src)
		return docExampleFile, err
	case docLambda.MatchString(strings.TrimSpace(src)):
		_, err := langparser.ParseV1Lambda(strings.TrimSpace(src))
		return docExampleLambda, err
	default:
		return docExampleFragment, nil
	}
}

// docLambda matches a block that is one lambda and nothing else.
var docLambda = regexp.MustCompile(`^\(?[A-Za-z_][A-Za-z0-9_, ]*\)?\s*=>`)

// firstSignificantWord returns the first word of the first line that is not
// blank, not a comment and not an annotation -- the word that says what the
// block is.
func firstSignificantWord(src string) string {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "",
			strings.HasPrefix(line, "//"),
			strings.HasPrefix(line, "#"),
			strings.HasPrefix(line, "@"),
			strings.HasPrefix(line, "/*"),
			strings.HasPrefix(line, "*"):
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "use" {
			continue // a file-top import; the declaration follows it.
		}
		return fields[0]
	}
	return ""
}

// isConstructKeyword reports whether a word opens a top-level declaration,
// read from the parser's own construct set rather than from a list here.
func isConstructKeyword(word string) bool {
	for _, kw := range langparser.ConstructKeywords() {
		if kw == word {
			return true
		}
	}
	return false
}

// fenceLocation is the `file:line` a failure names, pointing at the fence the
// block opens with.
func fenceLocation(file string, line int) string {
	return file + ":" + itoa(line)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// firstLine is the opening line of a block, for the skip log: enough to find
// the block on the page without printing it.
func firstLine(src string) string {
	for _, line := range strings.Split(src, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
