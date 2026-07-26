package memql

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// server_only_gate_parity_test.go -- memql#2875, the drift half.
//
// The REAL fix for #2875 lands in dsl/server_only_parsed_test.go: the audit
// derives its `@serverOnly` verdict from the PARSED tree (via
// `dslimports.Load`, which is importable from package `dsl` and already used
// there), applying the loader's own rule -- `hasFlagAttribute(attrs,
// "serverOnly")` -- to the same parse. So "the gate exempted it" and "the
// runtime enforces it" cannot diverge by construction.
//
// This file covers what that cannot: `sdk/gen/gen.go` still decides by REGEX
// over source, and it must, because it is a GENERATOR that runs over an
// arbitrary `--dsl` root without booting an engine. Its regex is therefore the
// last place the two verdicts can disagree.
//
// # Scope, stated rather than overclaimed
//
// This asserts the PATTERN TEXT at the source-scanning site, not a verdict over
// every construct kind. An earlier version of this file claimed to "cover all
// three sites at once" via a tree-wide regex/registry comparison. Two problems
// with that, both found in review:
//
//   - it regexed the whole SLICE (preamble + body + prepended `use` lines)
//     while the sites regex the PREAMBLE only, so an `@serverOnly` inside a
//     body annotation string -- e.g. an args field's multi-line
//     `@description` -- was reported as FAIL-OPEN with three false claims
//     attached, on a construct nothing had exempted;
//   - the registry only holds query / mutation / logic, so the comparison was
//     blind to the 185 `seed` and 74 `builtin` constructs the source-scanning
//     sites also read. The parsed set in dsl/ walks 650 named declarations and
//     covers those.
//
// A genuine `@serverOnly` is rejected at parse on both seeds ("unknown seed
// annotation") and builtins ("unknown annotation ... supported: @alias, @args,
// ..."), so only the smuggled form is reachable there, and its blast radius is
// an audit exemption or an SDK omission rather than a callable-with-no-scope
// construct.

// serverOnlyRegexSource is the pattern sdk/gen compiles. Duplicated here on
// purpose: the point is to detect drift, so importing the site's variable would
// make the test blind to a change in it.
const serverOnlyRegexSource = `(?m)^@serverOnly\b`

// TestServerOnlyRegexPatternIsTheOneSdkGenUses guards the premise: this file is
// only meaningful while the constant above is what the generator actually
// compiles.
//
// Read from source rather than imported, because the goal is to notice an edit
// to that literal -- importing the variable would make any change invisible by
// construction.
func TestServerOnlyRegexPatternIsTheOneSdkGenUses(t *testing.T) {
	const path = "../../sdk/gen/gen.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v -- if the generator moved, move this check with it; without it "+
			"the SDK's @serverOnly skip can drift from the parsed audit unnoticed", path, err)
	}
	decl := regexp.MustCompile(`serverOnlyRe\s*=\s*regexp\.MustCompile\(` + "`" + `([^` + "`" + `]*)` + "`" + `\)`)
	m := decl.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: could not find `serverOnlyRe = regexp.MustCompile(...)`. If the generator "+
			"now derives its verdict differently -- ideally from the parsed AST, which it can do "+
			"without an engine -- this check should be replaced rather than repaired.", path)
	}
	if got := string(m[1]); got != serverOnlyRegexSource {
		t.Errorf("%s compiles %q, this file expects %q. The generator's verdict has drifted from "+
			"the parsed audit in dsl/server_only_parsed_test.go; reconcile them.", path, got, serverOnlyRegexSource)
	}
}

// TestLoadedRegistryAgreesWithSdkGenOnTheRealTree is the narrow, honest version
// of the tree-wide comparison: for every construct the registry knows, the
// generator's regex over its PREAMBLE must match Function.ServerOnly.
//
// Preamble, not the whole slice -- that was review finding 2. The preamble is
// the only text the generator reads, so it is the only text a comparison
// against it may use.
func TestLoadedRegistryAgreesWithSdkGenOnTheRealTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	// The concept registry and the LoadReport are both passed deliberately.
	// Passing nil concepts loads FEWER functions (436 vs 445) -- a silent
	// partial load that would narrow the comparison without failing it. The
	// report is the instrument that catches a dropped slice, which the
	// checked==0 tripwire cannot.
	registry := newFunctionRegistry()
	report := newLoadReport()
	_, counts, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry(), report)
	if err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if report.HasProblems() {
		t.Fatalf("the function load reported problems, so the registry is incomplete and this "+
			"comparison would silently cover fewer constructs than it claims: %+v", report.Skipped)
	}

	re := regexp.MustCompile(serverOnlyRegexSource)
	sources := allConstructSources(logger)

	var mismatch []string
	checked, skipped, loaderYes := 0, 0, 0

	for _, fn := range registry.Snapshot() {
		if fn == nil || fn.Name == "" || fn.FunctionKind == "" {
			continue
		}
		src, ok := sources[fn.FunctionKind+"\x00"+fn.Name]
		if !ok {
			// Counted, and asserted on below. The previous version's comment
			// claimed this was "counted rather than skipped silently" while
			// providing no counter -- review finding 4.
			skipped++
			continue
		}
		checked++
		if fn.ServerOnly {
			loaderYes++
		}
		if re.MatchString(preambleOf(src)) != fn.ServerOnly {
			mismatch = append(mismatch, fn.Name+" ("+fn.FunctionKind+")")
		}
	}

	if checked == 0 {
		t.Fatal("compared 0 constructs -- this gate would pass vacuously (the #2799 meaningless-zero " +
			"failure mode). Check allConstructSources against FunctionKind values in the registry")
	}
	if skipped != 0 {
		t.Errorf("%d construct(s) in the registry had no recoverable source, so the generator's "+
			"regex cannot see them either and this comparison silently skipped them. The "+
			"checked==0 tripwire cannot detect a PARTIAL drop, which is why this is asserted.", skipped)
	}
	if loaderYes == 0 {
		t.Fatal("the loaded registry reports ZERO @serverOnly constructs -- either the annotation " +
			"has no live users, or hasFlagAttribute stopped seeing it, which would silently " +
			"disable the gate on every construct that carries it")
	}
	for _, name := range mismatch {
		t.Errorf("%s: sdk/gen's regex over the preamble disagrees with Function.ServerOnly.\n"+
			"\tWhichever way it points, the generated SDK and the runtime now disagree about\n"+
			"\twhether this construct is reachable from a client (memql#2875).", name)
	}

	// Cross-check the comparison covered the whole registry, not a subset.
	want := 0
	for _, n := range counts {
		want += n
	}
	if checked != want {
		t.Errorf("compared %d constructs but the loader reported %d -- the comparison is covering "+
			"a SUBSET, which narrows the gate without failing it", checked, want)
	}

	t.Logf("compared %d constructs (%d @serverOnly, %d skipped, loader reported %d)", checked, loaderYes, skipped, want)
}

// preambleOf returns the `@`-attribute / doc-comment run above a slice's
// construct header -- the same text sdk/gen's attrPreamble hands to its regex.
//
// A slice starts AT its preamble, so this is "everything up to the first line
// that is neither blank, an annotation, nor a comment". Matching the whole
// slice instead was review finding 2: an `@serverOnly` inside a BODY
// annotation string is text no gate ever reads, and reporting it as a
// divergence attaches three false claims to a construct nothing exempted.
func preambleOf(slice string) string {
	lines := strings.Split(slice, "\n")

	// Find the construct header. Everything before it is candidate preamble;
	// everything at or after it is body. This cannot be "the first
	// non-annotation line" -- ExtractFunctionSlices PREPENDS the file-top
	// `use ...` declarations to every slice, so a naive scan breaks on line 0
	// and returns an empty preamble, which silently reports every @serverOnly
	// construct as a mismatch. (It did, first try.)
	header := -1
	for i, line := range lines {
		if constructHeaderForPreamble.MatchString(line) {
			header = i
			break
		}
	}
	if header < 0 {
		return ""
	}

	// Walk back over the annotation / doc-comment run, exactly as sdk/gen's
	// attrPreamble does -- including that it accepts `@` and `///` and steps
	// over blank lines, but stops at anything else.
	first := header
	for i := header - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "///") {
			break
		}
		first = i
	}
	return strings.Join(lines[first:header], "\n")
}

// constructHeaderForPreamble matches a top-level construct header inside a
// slice, so preambleOf can tell preamble from body.
var constructHeaderForPreamble = regexp.MustCompile(`^[ \t]*(query|mutate|seed|logic|func)\b`)

// allConstructSources returns every function-registry construct's source, keyed
// "<kind>\x00<name>". One pass over the tree: calling DSLConstructSource per
// construct re-reads and re-slices the whole tree each time, measured at 25s
// for 445 constructs against 0.2s here.
//
// Only ExtractFunctionSlices is run. The earlier version also ran
// ExtractAutomationSlices, whose 31 entries can never be looked up because no
// registry construct has kind "automation" -- review finding 6.
func allConstructSources(logger *slog.Logger) map[string]string {
	out := map[string]string{}
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractFunctionSlices(raw.Content) {
			out[string(slice.Kind)+"\x00"+slice.Name] = slice.Source
		}
	}
	return out
}
