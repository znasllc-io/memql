package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// main_test.go pins the literal rewrite on the fixture shapes Go tests embed
// MemQL in. Each case is a whole Go file, processed the way the tool
// processes a file on disk.

var testPreds = map[string]langparser.PredicateInfo{"isNotDeleted": {}}

func process(t *testing.T, src string) fileReport {
	t.Helper()
	rep, err := processFile("fixture_test.go", []byte(src), testPreds)
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	return rep
}

// outcomes returns the file's results in order, as outcome names.
func outcomes(rep fileReport) []string {
	names := map[outcome]string{unchanged: "unchanged", changed: "changed", refused: "refused", fragment: "fragment", kept: "kept"}
	var out []string
	for _, r := range rep.results {
		out = append(out, names[r.outcome])
	}
	return out
}

func wantOutcomes(t *testing.T, rep fileReport, want ...string) {
	t.Helper()
	if got := strings.Join(outcomes(rep), ","); got != strings.Join(want, ",") {
		t.Fatalf("outcomes = %s, want %s", got, strings.Join(want, ","))
	}
}

// A raw literal holding a struct query: the filter moves to the lambda form,
// the literal stays raw, and every byte outside the clause -- the literal's
// own indentation included -- is as it was.
func TestRawLiteralQueryFilter(t *testing.T) {
	src := "package p\n\nconst q = `query thing listThings {\n" +
		"  args {\n    status string!\n  }\n" +
		"  filter  status == args.status && isNotDeleted\n" +
		"  paginate 10\n}\n`\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "changed")
	want := "package p\n\nconst q = `query thing listThings {\n" +
		"  args {\n    status string!\n  }\n" +
		"  filter  row => row.status == args.status && isNotDeleted(row)\n" +
		"  paginate 10\n}\n`\n"
	if string(rep.out) != want {
		t.Fatalf("rewritten file:\n%s\nwant:\n%s", rep.out, want)
	}
}

// An interpreted literal is re-quoted, escapes and all.
func TestInterpretedLiteralTrait(t *testing.T) {
	src := "package p\n\nvar s = \"trait isLive {\\n  return active == true\\n}\\n\"\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "changed")
	if want := "var s = \"trait isLive = row => row.active == true\\n\"\n"; !bytes.Contains(rep.out, []byte(want)) {
		t.Fatalf("rewritten file:\n%s\nwant it to contain:\n%s", rep.out, want)
	}
}

// A trigger filter in an automation fixture, and an in-process call form in a
// logic body.
func TestAutomationAndLogicForms(t *testing.T) {
	src := "package p\n\nconst a = `@trigger(event=\"node.created\", concept=\"v1:x:y\")\n" +
		"@filter(payload.kind == \"daily\")\n" +
		"automation onY {\n  step go {\n    logic doIt(x: 1)\n  }\n}\n`\n\n" +
		"const l = `logic pick {\n  body {\n    return cond(args.a > 1, \"big\", \"small\")\n  }\n}\n`\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "changed", "changed")
	for _, want := range []string{
		`@filter(row => row.kind == "daily")`,
		`return args.a > 1 ? "big" : "small"`,
	} {
		if !bytes.Contains(rep.out, []byte(want)) {
			t.Errorf("rewritten file lacks %s:\n%s", want, rep.out)
		}
	}
}

// A trait declared in one literal resolves a bare conjunct in another: the
// file's own declarations are laid over the corpus set.
func TestLocalPredicateResolves(t *testing.T) {
	src := "package p\n\nconst tr = `trait isVip {\n  return tier == \"vip\"\n}\n`\n\n" +
		"const q = `query account vips {\n  filter isVip && region == \"eu\"\n  paginate 5\n}\n`\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "changed", "changed")
	if !bytes.Contains(rep.out, []byte(`filter row => isVip(row) && row.region == "eu"`)) {
		t.Fatalf("the local trait did not resolve:\n%s", rep.out)
	}
}

// What the rewrite cannot do is reported, and the literal is left as it was:
// a clause the rewrite refuses, and a fragment whose construct is assembled
// from several literals.
func TestRefusalAndFragmentAreReportedNotGuessed(t *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\n" +
		"var q = fmt.Sprintf(`query thing t {\n  filter  status == %q\n  paginate 5\n}\n`, \"x\")\n\n" +
		"var header = \"query thing t {\\n\"\n" +
		"var clause = \"  filter status == \\\"x\\\"\\n\"\n"
	rep := process(t, src)
	// The header literal alone holds nothing legacy, so it is simply unchanged;
	// the clause literal after it is the fragment.
	wantOutcomes(t, rep, "refused", "unchanged", "fragment")
	if rep.out != nil {
		t.Fatalf("a file with nothing rewritable was rewritten:\n%s", rep.out)
	}
	if !strings.Contains(rep.results[0].reason, "legacy grammar does not read it") {
		t.Errorf("refusal reason = %q", rep.results[0].reason)
	}
	if !strings.Contains(rep.results[2].reason, "split across literals") {
		t.Errorf("fragment reason = %q", rep.results[2].reason)
	}
}

// A literal already in the v1 form, and a literal that merely mentions the
// word "filter", are both left alone and unreported.
func TestV1AndProseLiteralsAreLeftAlone(t *testing.T) {
	src := "package p\n\nconst q = `query thing t {\n  filter row => row.status == \"x\"\n  paginate 5\n}\n`\n\n" +
		"const msg = \"filter evaluation failed: the list is empty\"\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "unchanged")
	if rep.out != nil {
		t.Fatal("an already-v1 fixture was rewritten")
	}
}

// The keep markers: on the literal's line or the line above, and for the
// whole file.
func TestKeepMarkers(t *testing.T) {
	literal := "`query thing t {\n  filter status == \"x\"\n  paginate 5\n}\n`"
	above := "package p\n\n// This fixture pins the legacy grammar. memqlmigrate:keep\nconst q = " + literal + "\n"
	wantOutcomes(t, process(t, above), "kept")
	file := "package p\n\n// memqlmigrate:keep-file -- the legacy refusal messages are the subject.\n\nconst q = " + literal + "\n"
	wantOutcomes(t, process(t, file), "kept")
}

// A second run over a rewritten file changes nothing, and the rewritten file
// is still Go.
func TestIdempotentAndStillGo(t *testing.T) {
	src := "package p\n\nconst q = `query thing t {\n  filter status == \"x\" && when(args.since) { updatedAt >= args.since }\n  paginate 5\n}\n`\n" +
		"var s = \"spec thing isOpen {\\n  return status == \\\"open\\\"\\n}\\n\"\n"
	first := process(t, src)
	wantOutcomes(t, first, "changed", "changed")
	if _, err := parser.ParseFile(token.NewFileSet(), "x.go", first.out, 0); err != nil {
		t.Fatalf("the rewritten file is not Go: %v\n%s", err, first.out)
	}
	second, err := processFile("fixture_test.go", first.out, map[string]langparser.PredicateInfo{"isOpen": {}})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	wantOutcomes(t, second, "unchanged", "unchanged")
	if second.out != nil {
		t.Fatalf("the second run changed the file:\n%s", second.out)
	}
}

// Only *_test.go files are scanned (non-test files with -survey), and an
// exclude prefix is honoured.
func TestGoFilesSelection(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a_test.go", "b.go", filepath.Join("skip", "c_test.go"), filepath.Join("testdata", "d_test.go")} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests, err := goFiles([]string{dir}, options{excludes: []string{filepath.ToSlash(filepath.Join(dir, "skip")) + "/"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 1 || filepath.Base(tests[0]) != "a_test.go" {
		t.Fatalf("test files = %v, want only a_test.go", tests)
	}
	nonTest, err := goFiles([]string{dir}, options{survey: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(nonTest) != 1 || filepath.Base(nonTest[0]) != "b.go" {
		t.Fatalf("survey files = %v, want only b.go", nonTest)
	}
}
