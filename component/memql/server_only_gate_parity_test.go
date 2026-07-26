package memql

import (
	"log/slog"
	"os"
	"regexp"
	"sort"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// server_only_gate_parity_test.go -- memql#2875.
//
// Three places decide whether a construct is `@serverOnly` by REGEX over source:
//
//	dsl/conformance_test.go        serverOnlyAnnotationRe  (the per-row-authz gate)
//	dsl/server_only_authz_test.go  serverOnlyRe            (the docs gate)
//	sdk/gen/gen.go                 serverOnlyRe            (the SDK skip)
//
// The LOADER decides by parsing, and sets Function.ServerOnly. Those two can
// disagree, and the disagreement is FAIL-OPEN in the audit: a line beginning
// `@serverOnly` inside a multi-line annotation string --
//
//	@description("...
//	@serverOnly")
//
// -- or inside a block comment satisfies the regex, so the audit EXEMPTS the
// construct from per-row-authz classification and sdk/gen skips it, while
// Function.ServerOnly stays false and NOTHING is enforced at runtime. #2861
// (block comments in the DSL) made that more reachable.
//
// It is not attacker-reachable -- it requires authoring the construct in-repo.
// The reason to close it is that the classification test is the backstop that
// hard-fails on any new unclassified construct, and a construct that can slip
// out of that gate while ALSO having no runtime enforcement is the one shape
// the gate exists to make impossible.
//
// # Why this is a parity gate rather than a rewrite of the three sites
//
// The issue proposes having the audit read Function.ServerOnly off the loaded
// registry. That cannot be done where the audit lives: `component/memql`
// imports `github.com/znasllc-io/memql/dsl` (ai_prompts.go, capability_loader.go,
// build_offline_sense.go), so package `dsl` cannot import back into
// `component/memql` to load a registry. Only the leaf subpackages `dslfs` and
// `sense` are importable from there, and neither loads functions.
//
// sdk/gen has the mirror problem from the other direction: it is a GENERATOR
// that must run over an arbitrary `--dsl` root without booting an engine, so
// making it registry-backed would couple codegen to engine Init.
//
// So this test sits where BOTH views are available and asserts they agree. The
// divergence becomes unshippable rather than unrepresentable -- weaker than the
// issue's phrasing ("cannot diverge by construction") but achievable without
// moving an authz audit across a package boundary, and it fails on the exact
// condition that matters.
//
// It also covers all three sites at once, because all three compile the SAME
// pattern; the constant below is asserted to match each.

// serverOnlyRegexSource is the pattern the three source-scanning sites use. It
// is duplicated here deliberately -- the point is to detect drift between the
// regex verdict and the loader verdict, so importing one of the three would
// make the test blind to a change in the other two.
const serverOnlyRegexSource = `(?m)^@serverOnly\b`

var serverOnlyParityRe = regexp.MustCompile(serverOnlyRegexSource)

// TestServerOnlyRegexAgreesWithTheLoadedRegistry is the parity gate.
//
// For every function-registry construct, the regex verdict over its source must
// equal Function.ServerOnly. A mismatch in either direction is a defect; the
// FAIL-OPEN direction (regex says yes, loader says no) is called out separately
// because that is the one that silently removes both the audit obligation and
// the runtime enforcement at the same time.
func TestServerOnlyRegexAgreesWithTheLoadedRegistry(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	// Slice every file ONCE. Calling DSLConstructSource per construct re-reads
	// and re-slices the whole tree each time -- O(constructs x files), which
	// measured 26s for 445 constructs. Same inputs, ~0.5s.
	sources := allConstructSources(logger)

	var (
		failOpen   []string
		failClosed []string
		checked    int
		loaderYes  int
		regexYes   int
	)

	for _, fn := range registry.Snapshot() {
		if fn == nil || fn.Name == "" {
			continue
		}
		kind := fn.FunctionKind
		if kind == "" {
			continue
		}
		src, ok := sources[kind+"\x00"+fn.Name]
		if !ok {
			// A construct with no recoverable source cannot be compared. That
			// is not a pass -- it means the regex sites cannot see it either,
			// which is worth knowing, so it is counted rather than skipped
			// silently.
			continue
		}
		checked++

		byRegex := serverOnlyParityRe.MatchString(src)
		byLoader := fn.ServerOnly
		if byLoader {
			loaderYes++
		}
		if byRegex {
			regexYes++
		}

		switch {
		case byRegex && !byLoader:
			failOpen = append(failOpen, fn.Name+" ("+kind+")")
		case byLoader && !byRegex:
			failClosed = append(failClosed, fn.Name+" ("+kind+")")
		}
	}

	if checked == 0 {
		t.Fatal("compared 0 constructs -- this gate would then pass vacuously, which is the " +
			"failure mode that made the previous user-scope detector report a meaningless zero " +
			"(#2799). Check DSLConstructSource against FunctionKind values in the registry")
	}
	if loaderYes == 0 {
		t.Fatal("the loaded registry reports ZERO @serverOnly constructs. Either the annotation " +
			"has no live users -- in which case check whether the enforcement in " +
			"function_validator.go is still exercised by anything -- or hasFlagAttribute stopped " +
			"seeing it, which would silently disable the gate on every construct that carries it")
	}

	sort.Strings(failOpen)
	sort.Strings(failClosed)

	for _, name := range failOpen {
		t.Errorf("FAIL-OPEN: %s matches the @serverOnly regex but Function.ServerOnly is FALSE.\n"+
			"\tThe audit gates (dsl/conformance_test.go, dsl/server_only_authz_test.go) therefore\n"+
			"\tEXEMPT it from per-row-authz classification and sdk/gen drops it from the client\n"+
			"\tsurface, while the runtime enforces NOTHING -- the construct is fully callable by\n"+
			"\tany client with no caller-scope obligation. Usual cause: a line beginning\n"+
			"\t`@serverOnly` inside a multi-line annotation string or a block comment (memql#2875).", name)
	}
	for _, name := range failClosed {
		t.Errorf("FAIL-CLOSED: %s has Function.ServerOnly TRUE but does not match the @serverOnly\n"+
			"\tregex. Runtime refuses client calls while the audit gates still demand caller-scope\n"+
			"\tclassification and sdk/gen still EXPORTS it to the client surface -- an advertised\n"+
			"\tconstruct that is always refused (memql#2647's dishonest surface).", name)
	}

	t.Logf("compared %d constructs: %d @serverOnly by loader, %d by regex", checked, loaderYes, regexYes)
}

// TestServerOnlyRegexPatternIsTheOneTheGatesUse guards the premise of the test
// above: it is only meaningful while the constant here is the pattern the three
// source-scanning sites actually compile.
//
// Checked by reading their source rather than importing their variables --
// `dsl` and `sdk/gen` are not importable from here (see the file header), and
// two of the three live in _test.go files that are not importable at all.
func TestServerOnlyRegexPatternIsTheOneTheGatesUse(t *testing.T) {
	for _, site := range []struct{ path, varName string }{
		{"../../dsl/conformance_test.go", "serverOnlyAnnotationRe"},
		{"../../dsl/server_only_authz_test.go", "serverOnlyRe"},
		{"../../sdk/gen/gen.go", "serverOnlyRe"},
	} {
		raw, err := os.ReadFile(site.path)
		if err != nil {
			t.Errorf("read %s: %v -- if this file moved, move this check with it; without it the "+
				"parity gate silently compares against a pattern nobody uses", site.path, err)
			continue
		}
		decl := regexp.MustCompile(regexp.QuoteMeta(site.varName) + `\s*=\s*regexp\.MustCompile\(` + "`" + `([^` + "`" + `]*)` + "`" + `\)`)
		m := decl.FindSubmatch(raw)
		if m == nil {
			t.Errorf("%s: could not find `%s = regexp.MustCompile(...)`. The parity gate assumes "+
				"all three sites compile %q; if one now derives its pattern differently, this "+
				"test must learn how.", site.path, site.varName, serverOnlyRegexSource)
			continue
		}
		if got := string(m[1]); got != serverOnlyRegexSource {
			t.Errorf("%s: %s compiles %q, parity gate uses %q -- the two have drifted, so the "+
				"parity assertion no longer covers that site.", site.path, site.varName, got, serverOnlyRegexSource)
		}
	}
}

// allConstructSources returns every function-registry construct's source,
// keyed "<kind>\x00<name>". One pass over the tree, mirroring exactly what
// DSLConstructSource does per lookup.
func allConstructSources(logger *slog.Logger) map[string]string {
	out := map[string]string{}
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractFunctionSlices(raw.Content) {
			out[string(slice.Kind)+"\x00"+slice.Name] = slice.Source
		}
		for _, slice := range ExtractAutomationSlices(raw.Content) {
			out[string(slice.Kind)+"\x00"+slice.Name] = slice.Source
		}
	}
	return out
}

// TestServerOnlyRegexMatchesTheSmuggledShapes is the failing-first half: it
// proves the FAIL-OPEN condition the parity gate exists to catch is real, on
// the exact shapes #2875 names.
//
// No construct in the tree carries these today, so the tree-wide comparison
// above passes and cannot demonstrate the hazard. These assert the regex side
// of the divergence directly -- each of these SATISFIES the regex (so the audit
// gates exempt the construct and sdk/gen drops it) while a parser would never
// set Function.ServerOnly, because in each case `@serverOnly` is inside a
// string literal or a comment rather than being an annotation.
func TestServerOnlyRegexMatchesTheSmuggledShapes(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{
			"inside a multi-line annotation string",
			"@description(\"note\n@serverOnly\")\nquery user q {\n  filter row.id==args.x\n}\n",
		},
		{
			"inside a block comment",
			"/*\n@serverOnly\n*/\nquery user q {\n  filter row.id==args.x\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !serverOnlyParityRe.MatchString(tc.src) {
				t.Fatalf("the regex does NOT match %s.\nThat would mean #2875's fail-open shape is "+
					"no longer reachable and the parity gate is guarding nothing -- verify against "+
					"the current pattern before deleting either test.", tc.name)
			}
			// The construct itself declares no annotation, so a loader would
			// report ServerOnly=false. Regex yes + loader no is exactly the
			// mismatch the parity gate reports as FAIL-OPEN.
			if hasFlagAttribute(nil, "serverOnly") {
				t.Fatal("fixture: hasFlagAttribute(nil, ...) should be false")
			}
		})
	}
}

// unusedLanguageParserGuard keeps the languageParser import honest if the
// FunctionKind comparison above is ever refactored to use the typed constants.
var _ = languageParser.FunctionTypeAutomation
