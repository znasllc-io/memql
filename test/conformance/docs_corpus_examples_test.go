package conformance

// The docs cannot show a form the ENGINE refuses (memql#5388, D24's second
// half).
//
// docs_examples_test.go PARSES every ```memql fence under
// docs/public/language. That closes one gap and leaves two open, and this file
// closes those:
//
//   - Parsing is not loading. A fence can parse cleanly and still name a
//     construct that does not exist, project a field its concept does not
//     declare, read the actor without declaring `@actor`, or reach across a
//     namespace with no `use` import. The engine refuses every one of those at
//     LOAD; a parse says nothing about them. Measured on 2026-09-20 by lifting
//     the fences of docs/public/language/functions.md into the corpus
//     unchanged: 16 of the 30 cases this file now holds were refused by the
//     engine while both parse-only gates stayed green.
//   - A `fragment` is skipped, and fragments were half the corpus of examples
//     (69 of 135 blocks).
//
// # The marker
//
// Every ```memql fence on a covered page carries, on the line DIRECTLY above
// it, a marker naming the conformance case it is held to:
//
//	<!-- corpus: 2026/examples/<page>/<file>.memql -->
//	```memql
//	...
//	```
//
// The path is relative to test/conformance, and the case is an ordinary corpus
// case: it has an expect.json verdict, it may carry a fixture, and
// TestCorpusVerdicts runs the engine over it like any other. That is what makes
// the marker a claim about the engine rather than about a text file.
//
// # Three fence kinds, three questions
//
//	```memql            the case's verdict ACCEPTS, and the fence body EQUALS
//	                    the case file: the reader is looking at a whole file
//	                    the engine loads.
//	```memql fragment   the case's verdict ACCEPTS, and the fence body is a
//	                    contiguous SUBSTRING of the case file: the reader is
//	                    looking at part of a whole file the engine loads.
//	```memql retired    the case's verdict REFUSES, and the fence body EQUALS
//	                    the case file: the page says "this form is refused",
//	                    and the corpus is where that is checked.
//
// A fence with no marker, a marker naming a path no case names, or a verdict
// that contradicts the fence's kind, fails -- naming the page and the line the
// fence opens on.
//
// # Why a marker rather than transclusion
//
// docs_examples_test.go argued against drawing the examples from the corpus on
// the grounds that it would mean "either rewriting every page as a
// transclusion or copying files around, and both make the pages worse to read".
// The first half of that is right and this gate does not do it: the page still
// holds the example, in full, where a reader sees it. The second half is what
// the marker answers -- the copy is not unchecked, because `-update-docs`
// rewrites a bare or retired fence's body FROM its case file, so the case is
// the original and the page is a rendering of it. A fragment is not rewritten
// (there is no way to know which part of the file it wanted), only checked.
//
// # Normalization
//
// Both sides are normalized the same way before they are compared: CRLF to LF,
// trailing whitespace trimmed per line, leading and trailing blank lines
// trimmed. Leading indentation is NOT stripped -- a fragment of a construct
// body carries the indentation it has inside the file, which is what makes
// "contiguous substring" mean what it says.
//
// # The covered set is a list, and nothing is left out of it
//
// docsCorpusCoverage below is the one place that says which pages this gate
// holds. Every page under docs/public/language that carries a ```memql fence
// is in it, and every entry is now `true`: the two follow-ups the list was
// written with -- memql.md and authoring-rules.md -- have landed. The list
// stays because it is what makes a NEW page fail rather than pass silently:
// a page with a fence and no entry at all fails this gate, so the decision to
// cover it (or not yet) has to be written down.
//
// Every page keeps its existing cover as well: TestDocsExamplesParse reads
// every page in the tree regardless of this list, and nothing here weakens it.

import (
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// updateDocs rewrites every bare and retired fence body on a covered page from
// the case file its marker names. It never invents a marker: a fence with none
// is a decision about which case it belongs to, and a flag cannot make it.
var updateDocs = flag.Bool("update-docs", false,
	"rewrite bare and retired ```memql fence bodies under docs/public/language from the corpus cases their markers name")

// docsCorpusCoverage is THE list of pages this gate covers. Every page under
// docs/public/language holding a ```memql fence appears here, and a page this
// list does not name fails the gate rather than being skipped -- which is what
// a new page has to walk into. An entry set to false would be a page staged but
// not yet held; there are none today.
var docsCorpusCoverage = map[string]bool{
	"first-program.md":      true,
	"research-workflow.md":  true,
	"naming-conventions.md": true,
	"reserved.md":           true,
	"specifications.md":     true,
	"functions.md":          true,
	"memql.md":              true,
	"authoring-rules.md":    true,
}

// docsCorpusRoot is where a marker's path is resolved from: this package's
// directory, so a marker reads as the path a person would type.
const docsCorpusRoot = "."

// docsCorpusMarker matches the marker line and captures the case path.
var docsCorpusMarker = regexp.MustCompile(`<!--\s*corpus:\s*(\S+?)\s*-->`)

// docsCorpusAcceptingVerdicts are the verdicts under which the engine READ and
// ACCEPTED the case file. A fence a reader may copy must be held to one of
// these; lower/evaluate cases are bare expressions rather than files, so they
// are not among them.
var docsCorpusAcceptingVerdicts = map[string]bool{verdictLoadOK: true}

// docsCorpusRefusingVerdicts are the verdicts under which the engine REFUSED
// the case file -- what a ```memql retired fence claims.
var docsCorpusRefusingVerdicts = map[string]bool{verdictRefuseParse: true, verdictRefuseLoad: true}

// TestDocsExamplesComeFromTheCorpus holds every ```memql fence on a covered
// page to the conformance case its marker names.
func TestDocsExamplesComeFromTheCorpus(t *testing.T) {
	verdicts := docsCorpusVerdicts(t)
	pages := docsCorpusPages(t)
	if len(pages) == 0 {
		t.Fatal("no page under " + docsLanguageDir + " holds a ```memql fence: this gate would pass over a docs " +
			"tree with every example deleted, which is the fail-open shape it exists to prevent")
	}

	claimed := map[string]bool{}
	covered, checked := 0, 0
	for _, page := range pages {
		on, listed := docsCorpusCoverage[page]
		if !listed {
			t.Errorf("%s holds a ```memql fence and docsCorpusCoverage does not name it.\n"+
				"Every page under %s is either covered by this gate or named as not yet covered -- a page "+
				"absent from that list is one nothing decided about.", page, docsLanguageDir)
			continue
		}
		if !on {
			continue
		}
		covered++
		checked += docsCorpusCheckPage(t, page, verdicts, claimed)
	}
	if covered == 0 || checked == 0 {
		t.Fatalf("this gate checked %d fence(s) on %d covered page(s): a gate that matches nothing passes over anything", checked, covered)
	}
	docsCorpusCheckEveryCaseIsShown(t, verdicts, claimed)
	t.Logf("docs examples held to the corpus: %d fence(s) across %d covered page(s); %d page(s) named as not yet covered",
		checked, covered, len(docsCorpusCoverage)-covered)
}

// docsCorpusCheckPage checks one page's fences and returns how many it checked.
// With -update-docs it rewrites the page's bare and retired fence bodies first.
func docsCorpusCheckPage(t *testing.T, page string, verdicts map[string]string, claimed map[string]bool) int {
	t.Helper()
	full := filepath.Join(docsLanguageDir, page)
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	if *updateDocs {
		if updated, n := docsCorpusRewrite(t, page, string(raw), verdicts); n > 0 {
			if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
				t.Fatalf("write %s: %v", page, err)
			}
			t.Logf("-update-docs: rewrote %d fence(s) on %s from their corpus cases", n, page)
			raw = []byte(updated)
		}
	}

	src := string(raw)
	checked := 0
	for _, loc := range docFence.FindAllStringSubmatchIndex(src, -1) {
		marker := strings.TrimSpace(src[loc[2]:loc[3]])
		body := src[loc[4]:loc[5]]
		if strings.TrimSpace(body) == "" {
			continue
		}
		checked++
		line := 1 + strings.Count(src[:loc[0]], "\n")
		where := fenceLocation(page, line)

		casePath := docsCorpusMarkerAbove(src, loc[0])
		if casePath == "" {
			t.Errorf("%s: a ```memql fence carries no corpus marker.\n\n"+
				"Every ```memql fence on a covered page names the conformance case the engine is held to for it, "+
				"on the line directly above the fence:\n\n"+
				"\t<!-- corpus: 2026/examples/%s/<file>.memql -->\n\n"+
				"Add the case under test/conformance/2026/examples/%s/ with its expect.json verdict, then name it here. "+
				"Parsing the block is not enough: a block that parses can still be refused at load.",
				where, strings.TrimSuffix(page, ".md"), strings.TrimSuffix(page, ".md"))
			continue
		}
		verdict, known := verdicts[casePath]
		if !known {
			t.Errorf("%s: the corpus marker names %q, and no case in the corpus names that file.\n"+
				"The path is relative to test/conformance, and the file must be listed in its directory's expect.json.",
				where, casePath)
			continue
		}
		claimed[casePath] = true
		docsCorpusCompare(t, where, marker, casePath, verdict, body)
	}
	return checked
}

// docsCorpusCompare holds one fence to its case, by the rule its marker names.
func docsCorpusCompare(t *testing.T, where, marker, casePath, verdict, body string) {
	t.Helper()
	if msg := docsCorpusKindAgrees(marker, verdict); msg != "" {
		t.Errorf("%s: the fence names case %s, and %s", where, casePath, msg)
		return
	}
	caseSrc := docsCorpusReadCase(t, casePath)
	fence, file := docsCorpusNormalize(body), docsCorpusNormalize(caseSrc)

	if marker == "fragment" {
		if !strings.Contains(file, fence) {
			t.Errorf("%s: a ```memql fragment fence is not a contiguous part of its case %s.\n\n"+
				"--- the fence ---\n%s\n--- the case ---\n%s\n"+
				"The fence body must appear in the case file verbatim, indentation included -- that is what makes "+
				"the excerpt a thing the engine has actually loaded. Either edit the fence to quote the file, or "+
				"edit the case so it holds what the page wants to show.", where, casePath, fence, file)
		}
		return
	}
	kind := "bare"
	if marker == "retired" {
		kind = "retired"
	}
	docsCorpusRequireEqual(t, where, casePath, fence, file, kind)
}

// docsCorpusKindAgrees is the pure half of the check: does a fence of this kind
// agree with a case of this verdict? It returns "" when they do, and the
// sentence the failure ends with when they do not.
//
// It is separate from the comparison so the three-way decision can be tested
// without a page (TestDocsCorpusFenceKindDecision). The `retired` arm has no
// live example among the covered pages today, and an arm that never fires is
// an arm nothing has ever checked.
func docsCorpusKindAgrees(marker, verdict string) string {
	switch marker {
	case "", "fragment":
		if docsCorpusAcceptingVerdicts[verdict] {
			return ""
		}
		what := "a bare fence is a whole file a reader may copy"
		if marker == "fragment" {
			what = "a fragment is part of a file a reader may copy"
		}
		return fmt.Sprintf("its verdict is %q: %s, so its case must be a %s case. "+
			"Mark the fence `retired` if the page is showing a form the engine refuses.", verdict, what, verdictLoadOK)
	case "retired":
		if docsCorpusRefusingVerdicts[verdict] {
			return ""
		}
		return fmt.Sprintf("its verdict is %q: a `retired` fence claims the engine REFUSES the form, so its case is "+
			"a %s or %s case naming the refusal's message (and its rule code, when it carries one). "+
			"A verdict of %q says the engine accepts it.", verdict, verdictRefuseParse, verdictRefuseLoad, verdict)
	default:
		return fmt.Sprintf("```memql %s is not a fence kind -- the three are the bare fence, `fragment` and `retired`", marker)
	}
}

// docsCorpusRequireEqual fails when a whole-file fence and its case differ,
// showing the first line they disagree on rather than two blocks to diff by
// eye.
func docsCorpusRequireEqual(t *testing.T, where, casePath, fence, file, kind string) {
	t.Helper()
	if fence == file {
		return
	}
	t.Errorf("%s: a ```memql %s fence does not match its case %s.\n\n%s\n"+
		"A %s fence is the whole case file. Run\n\n"+
		"\tgo test ./test/conformance/ -run TestDocsExamplesComeFromTheCorpus -update-docs\n\n"+
		"to rewrite the page from the case, or edit the case if the page is the one that is right "+
		"(the case has to keep reaching its verdict).",
		where, kind, casePath, docsCorpusFirstDifference(fence, file), kind)
}

// docsCorpusFirstDifference names the first line the two sides disagree on.
func docsCorpusFirstDifference(fence, file string) string {
	a, b := strings.Split(fence, "\n"), strings.Split(file, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at line %d:\n\tpage: %q\n\tcase: %q", i+1, x, y)
		}
	}
	return "the two differ in trailing content only"
}

// docsCorpusRewrite replaces every bare and retired fence body on a page with
// its case file, and returns the page and how many it replaced. A fragment is
// left alone: nothing here knows which part of the file it meant to quote.
func docsCorpusRewrite(t *testing.T, page, src string, verdicts map[string]string) (string, int) {
	t.Helper()
	locs := docFence.FindAllStringSubmatchIndex(src, -1)
	var out strings.Builder
	at, n := 0, 0
	for _, loc := range locs {
		marker := strings.TrimSpace(src[loc[2]:loc[3]])
		if marker == "fragment" {
			continue
		}
		casePath := docsCorpusMarkerAbove(src, loc[0])
		if casePath == "" || !docsCorpusRewritable(marker, verdicts[casePath]) {
			continue
		}
		body := docsCorpusNormalize(docsCorpusReadCase(t, casePath)) + "\n"
		if body == src[loc[4]:loc[5]] {
			continue
		}
		out.WriteString(src[at:loc[4]])
		out.WriteString(body)
		at = loc[5]
		n++
	}
	if n == 0 {
		return src, 0
	}
	out.WriteString(src[at:])
	return out.String(), n
}

// docsCorpusRewritable reports whether a fence of this kind, naming a case with
// this verdict, is one -update-docs may write over. A mismatch is left for the
// check to report: rewriting it would hide the disagreement rather than fix it.
func docsCorpusRewritable(marker, verdict string) bool {
	switch marker {
	case "":
		return docsCorpusAcceptingVerdicts[verdict]
	case "retired":
		return docsCorpusRefusingVerdicts[verdict]
	}
	return false
}

// docsCorpusMarkerAbove returns the case path named on the line DIRECTLY above
// a fence, or "". Directly is the whole rule: a marker further up the page
// belongs to some other fence, and a fence whose marker is not adjacent is a
// fence with no marker.
func docsCorpusMarkerAbove(src string, fenceAt int) string {
	before := strings.TrimRight(src[:fenceAt], "\n")
	line := before
	if i := strings.LastIndexByte(before, '\n'); i >= 0 {
		line = before[i+1:]
	}
	m := docsCorpusMarker.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return ""
	}
	return path.Clean(m[1])
}

// docsCorpusNormalize is the one normalization both sides go through: CRLF to
// LF, trailing whitespace off each line, leading and trailing blank lines off
// the block. Leading indentation stays, so a fragment quotes the file's own
// shape.
func docsCorpusNormalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

func docsCorpusReadCase(t *testing.T, casePath string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(docsCorpusRoot, filepath.FromSlash(casePath)))
	if err != nil {
		t.Fatalf("read corpus case %s: %v", casePath, err)
	}
	return string(raw)
}

// docsCorpusVerdicts maps every case file in the corpus, by its path relative
// to this package, to the verdict its expect.json names. It reads the corpus
// through the runner's own reader, so this gate and TestCorpusVerdicts cannot
// disagree about what a case is.
func docsCorpusVerdicts(t *testing.T) map[string]string {
	t.Helper()
	root := os.DirFS(docsCorpusRoot)
	editions, err := corpusEditions(root)
	if err != nil {
		t.Fatalf("read test/conformance: %v", err)
	}
	out := map[string]string{}
	for _, edition := range editions {
		dirs, err := corpusExpectDirsIn(root, edition)
		if err != nil {
			t.Fatalf("walk %s: %v", edition, err)
		}
		for _, dir := range dirs {
			ef, err := readCorpusExpect(root, dir)
			if err != nil {
				t.Fatalf("%v", err)
			}
			for _, c := range ef.Cases {
				out[dir+"/"+c.File] = c.Verdict
			}
		}
	}
	return out
}

// docsCorpusPages lists the pages under docs/public/language that hold at least
// one ```memql fence, in name order.
func docsCorpusPages(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(docsLanguageDir)
	if err != nil {
		t.Fatalf("read %s: %v", docsLanguageDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(docsLanguageDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if docFence.MatchString(string(raw)) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// docsCorpusCheckEveryCaseIsShown fails on a case under 2026/examples/ that no
// fence names. The examples tree exists to back the pages: a case there that
// nothing shows is either a page that lost its marker or a case nobody deleted,
// and both leave the tree saying something it does not mean.
//
// The ONE exemption is a case staged under a page this gate does not read yet,
// and it is keyed on the coverage list holding that page AT ALL rather than on
// it being false. Keyed on false alone, `examples/functionz/` -- a directory
// named after a page that does not exist -- would be exempt too, since a map
// lookup of a name nobody listed is false as well, and a typo would buy a case
// silence instead of a failure.
func docsCorpusCheckEveryCaseIsShown(t *testing.T, verdicts map[string]string, claimed map[string]bool) {
	t.Helper()
	var orphans []string
	for casePath := range verdicts {
		if !strings.Contains(casePath, "/examples/") || claimed[casePath] {
			continue
		}
		if page := docsCorpusPageOf(casePath); page != "" {
			if on, listed := docsCorpusCoverage[page]; listed && !on {
				continue // a case staged for a page the gate does not read yet
			}
		}
		orphans = append(orphans, casePath)
	}
	sort.Strings(orphans)
	for _, o := range orphans {
		t.Errorf("corpus case %s is under examples/ and no ```memql fence on a covered page names it: "+
			"either a page dropped its marker, or the case outlived the example it was written for", o)
	}
}

// docsCorpusPageOf names the page a case under 2026/examples/ belongs to, from
// the first path segment under examples/, or "" when the path is not of that
// shape.
func docsCorpusPageOf(casePath string) string {
	_, rest, ok := strings.Cut(casePath, "/examples/")
	if !ok {
		return ""
	}
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return ""
	}
	return seg + ".md"
}

// ---- the published example folders ---------------------------------------

// Two of the pages this gate covers show an example that ALSO ships as a
// checked-in folder a reader can open and lint:
// examples/reading-list (first-program.md) and examples/research-desk
// (research-workflow.md). The corpus needs its own copy -- a case is a file in
// a case directory with an expect.json beside it, and the example folders are
// domains with their own memql.toml and namespace.pin that a reader browses --
// so there are two files either way.
//
// The choice this pins is that there are two files and ONE content. The
// alternative was to point the example's consumer at the corpus copy:
// component/automations/steps/research_example_test.go reads
// examples/research-desk/research/brief.memql by path and slices it on its
// `// showcase:<name>:start` markers. Re-pointing it at test/conformance would
// make an engine test read the conformance corpus to find the source it
// executes, which is a dependency the other way round from every other test in
// that package, and would leave the browsable example the one copy nothing
// checks. Byte identity is one assertion, it names both paths, and it cannot
// drift.
var publishedExampleCases = map[string]string{
	"../../examples/reading-list/reading.memql":         "2026/examples/first-program/reading-list.memql",
	"../../examples/research-desk/research/brief.memql": "2026/examples/research-workflow/brief.memql",
}

// TestPublishedExamplesMatchTheirCorpusCases fails when a checked-in example
// folder and the corpus case that holds it to the engine have drifted apart.
func TestPublishedExamplesMatchTheirCorpusCases(t *testing.T) {
	for example, casePath := range publishedExampleCases {
		published, err := os.ReadFile(filepath.FromSlash(example))
		if err != nil {
			t.Fatalf("read published example %s: %v", example, err)
		}
		if got, want := string(published), docsCorpusReadCase(t, casePath); got != want {
			t.Errorf("%s and %s have drifted apart.\n\n%s\n\n"+
				"They are one content in two places: the folder is what a reader opens and lints, the case is what "+
				"TestCorpusVerdicts runs the engine over, and the page's fences are checked against the case. "+
				"Copy whichever is right over the other -- and if it is the example that changed, the case has to "+
				"keep reaching its verdict.",
				example, casePath, docsCorpusFirstDifference(docsCorpusNormalize(got), docsCorpusNormalize(want)))
		}
	}
}

// TestDocsCorpusFenceKindDecision pins the three-way decision the marker makes.
// It predates any live `retired` fence and stays after them: the decision is a
// pure function of (marker, verdict), and the pages exercise the arms they
// happen to need rather than all of them.
func TestDocsCorpusFenceKindDecision(t *testing.T) {
	for _, tc := range []struct {
		marker, verdict string
		agrees          bool
	}{
		{"", verdictLoadOK, true},
		{"", verdictRefuseParse, false},
		{"", verdictRefuseLoad, false},
		{"", verdictLower, false},
		{"fragment", verdictLoadOK, true},
		{"fragment", verdictRefuseLoad, false},
		{"fragment", verdictEvaluate, false},
		{"retired", verdictRefuseParse, true},
		{"retired", verdictRefuseLoad, true},
		{"retired", verdictLoadOK, false},
		{"validated", verdictLoadOK, false}, // not one of the three kinds
	} {
		got := docsCorpusKindAgrees(tc.marker, tc.verdict)
		if agrees := got == ""; agrees != tc.agrees {
			t.Errorf("```memql %q naming a %s case: agrees=%v, want %v (%s)", tc.marker, tc.verdict, agrees, tc.agrees, got)
		}
	}
}

// TestDocsCorpusNormalizeAndSubstring pins what "equals" and "is a contiguous
// part of" mean here: the normalization both sides go through, and the reach
// and the LIMIT of keeping indentation.
//
// The limit is worth stating, because it is easy to overclaim. A ONE-LINE
// excerpt written flush left is still found -- the search is a substring
// search, and the file's indented line contains the dedented text. What the
// indentation actually buys is the MULTI-line case: the newline between two
// quoted lines makes the second line's indentation part of the match, so a
// block dedented as a block does not match the file it came from.
func TestDocsCorpusNormalizeAndSubstring(t *testing.T) {
	file := docsCorpusNormalize("\n\nmutation guide g {\r\n  insert {   \n    id: args.id\n  }\n}\n\n")
	if want := "mutation guide g {\n  insert {\n    id: args.id\n  }\n}"; file != want {
		t.Fatalf("normalize = %q, want %q", file, want)
	}
	if quoted := docsCorpusNormalize("  insert {\n    id: args.id\n  }\n"); !strings.Contains(file, quoted) {
		t.Errorf("a block quoted with the indentation it has in the file must be found in it")
	}
	if dedented := docsCorpusNormalize("insert {\n  id: args.id\n}\n"); strings.Contains(file, dedented) {
		t.Errorf("a block dedented as a block must NOT be found: its inner lines no longer line up with the file's")
	}
}
