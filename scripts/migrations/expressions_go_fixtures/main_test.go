package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// main_test.go pins the literal rewrite on the fixture shapes Go tests embed
// MemQL in. Each case is a whole Go file, processed the way the tool
// processes a file on disk.

// testTree stands in for the dsl/ tree a fixture's MemQL loads over: a trait,
// the core @actor shape, and the concept the fixtures bind.
var testTree = map[string][]byte{
	"common/traits.memql":  []byte("trait isNotDeleted {\n  return deleted != true\n}\n"),
	"common/shapes.memql":  []byte("/// The caller.\n@actor\nshape actorEnvelope {\n  actor.userId\n  actor.role\n}\n"),
	"todos/concepts.memql": []byte("concept todo {\n  ownerUserId string\n}\n"),
}

func testBase(t *testing.T) predicateBase {
	t.Helper()
	base, err := newPredicateBase(testTree)
	if err != nil {
		t.Fatalf("newPredicateBase: %v", err)
	}
	return base
}

func process(t *testing.T, src string) fileReport {
	t.Helper()
	rep, err := processFile("fixture_test.go", []byte(src), testBase(t))
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
	second, err := processFile("fixture_test.go", first.out, testBase(t))
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

// A fixture that declares a spec over the core @actor shape actorEnvelope,
// without declaring the shape, gets `= actor =>` and `isAdmin(actor)`: its
// declarations resolve over the tree it loads over. Before, the literal's
// declarations were collected on their own, the shape was nowhere, and the
// spec came out `= row => row.role` -- which a real engine refuses at define,
// and which a second run would see as v1 and keep.
func TestSpecOverACoreActorShape(t *testing.T) {
	src := "package p\n\nconst spec = `spec actorEnvelope isAdmin {\n  return role == \"admin\"\n}\n`\n\n" +
		"const q = `query todo mine {\n  filter isAdmin || ownerUserId == actor.userId\n}\n`\n"
	for name, base := range map[string]predicateBase{"the test tree": testBase(t), "the real dsl tree": realTree(t)} {
		t.Run(name, func(t *testing.T) {
			rep, err := processFile("fixture_test.go", []byte(src), base)
			if err != nil {
				t.Fatal(err)
			}
			wantOutcomes(t, rep, "changed", "changed")
			for _, want := range []string{
				"`spec actorEnvelope isAdmin = actor => actor.role == \"admin\"\n`",
				"filter row => isAdmin(actor) || row.ownerUserId == actor.userId",
			} {
				if !bytes.Contains(rep.out, []byte(want)) {
					t.Errorf("rewritten file lacks %s:\n%s", want, rep.out)
				}
			}
		})
	}
}

// A binding nothing declares -- not the file, not the tree, not a signature
// -- is refused and named, and the literal is left as it was.
func TestBindingNothingDeclaresIsRefused(t *testing.T) {
	rep := process(t, "package p\n\nconst s = `spec gadget isShiny {\n  return shiny == true\n}\n`\n")
	wantOutcomes(t, rep, "refused")
	if rep.out != nil {
		t.Fatalf("a refused literal was rewritten:\n%s", rep.out)
	}
	if want := "spec isShiny binds gadget, which no shape and no concept declares"; !strings.Contains(rep.results[0].reason, want) {
		t.Errorf("refusal reason = %q, want it to name the binding", rep.results[0].reason)
	}
}

// The legacy `<boundConcept>.<field>` spelling, as TestSandboxInheritsActorBinding
// (component/memql) writes it: `todo.ownerUserId` in a query bound to todo is
// `row.ownerUserId`, not `row.todo.ownerUserId`.
func TestBoundConceptPrefix(t *testing.T) {
	src := "package p\n\nvar c = struct{ Source string }{\n\tSource: `@actor\nquery todo sandboxOwned {\n  filter todo.ownerUserId == actor.userId\n}`,\n}\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "changed")
	if want := "`@actor\nquery todo sandboxOwned {\n  filter row => row.ownerUserId == actor.userId\n}`"; !bytes.Contains(rep.out, []byte(want)) {
		t.Fatalf("rewritten file:\n%s\nwant it to contain:\n%s", rep.out, want)
	}
}

// realTree is the repository's dsl/, the tree the tool reads by default.
func realTree(t *testing.T) predicateBase {
	t.Helper()
	base, err := loadPredicates([]string{filepath.Join("..", "..", "..", "dsl")})
	if err != nil {
		t.Fatalf("loadPredicates: %v", err)
	}
	if len(base.decls) < 100 {
		t.Fatalf("read %d files from dsl/: the real-tree case would prove nothing", len(base.decls))
	}
	if _, ok := base.preds["isActiveRecord"]; !ok {
		t.Fatal("the real tree has no isActiveRecord: the wrong tree was read")
	}
	return base
}

// Two fixtures of one file that declare one shape name with different kinds
// leave the file's declarations unusable; each refusal then says so, rather
// than pointing at the tree with "not in the predicate set".
func TestConflictingFileDeclarationsAreNamed(t *testing.T) {
	src := "package p\n\nconst a = `@actor\nshape labActor {\n  actor.role\n}\n`\n\n" +
		"const b = `@row\nshape labActor {\n  sku\n}\n`\n\n" +
		"const s = `spec labActor requiresAdmin {\n  return role == \"admin\"\n}\n`\n"
	rep := process(t, src)
	wantOutcomes(t, rep, "refused")
	if want := "this file's own declarations conflict"; !strings.Contains(rep.results[0].reason, want) || !strings.Contains(rep.results[0].reason, "labActor") {
		t.Errorf("refusal reason = %q, want it to name the conflict", rep.results[0].reason)
	}

	// Independent snippets that declare one name differently -- a spec foo
	// over the core @actor shape in one literal, a trait foo in another --
	// each answer for themselves.
	snippets := "package p\n\nconst a = `spec actorEnvelope foo {\n  return role == \"admin\"\n}\n`\n\n" +
		"const b = `trait foo {\n  return active == true\n}\n`\n"
	rep = process(t, snippets)
	wantOutcomes(t, rep, "changed", "changed")
	for _, want := range []string{"spec actorEnvelope foo = actor => actor.role == \"admin\"", "trait foo = row => row.active == true"} {
		if !bytes.Contains(rep.out, []byte(want)) {
			t.Errorf("rewritten file lacks %s:\n%s", want, rep.out)
		}
	}
}
