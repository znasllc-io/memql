package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMakefileRunPatternsMatchARealTest closes the class of defect that #2923
// is one instance of: a `go test -run <Pattern>` recipe whose pattern matches
// no test in the package it targets. `go test` exits 0 on that with
//
//	ok  	<pkg>	0.001s [no tests to run]
//
// which is indistinguishable from a pass in every log a human actually reads.
//
// #2923 was `arch-model-check` running `-run TestArchitectureModelIsCurrent`
// against component/architecture/, where the staleness gate is really named
// TestArchitectureModelIsNotStale (model_current_test.go:87).
//
// Be precise about the blast radius, because "a dead gate" invites overstating
// it: CI was never affected. ci.yml:144 runs `go test -count=1 ./...`, which
// runs every test in the package under its real name whatever the Makefile
// asks for, so PRs stayed gated throughout. The cost is local, and it lands on
// exactly the person doing the right thing -- running a *-check target before
// pushing to save a CI round-trip. What earns this a gate is that the failure
// is silent and the target's name promises the opposite.
//
// Two parsing decisions carry the weight here:
//
//   - The flag is anchored, per makeRunFlag above. A guard that reports prose
//     as a broken target is noise, and noisy guards get ignored.
//   - Test names are collected by reading every *_test.go in the package,
//     rather than by asking the toolchain with `go test -list`. -list is the
//     more authoritative answer but it honours build tags, so a pattern naming
//     a tagged test would come back matching nothing and this gate would fail
//     a Makefile that works. Reading the files is deliberately permissive: it
//     fires only when no file in the package declares a matching name at all.
//     A gate that blocks valid work is worse than the invisible target it
//     replaces.
//
// runFinding is one problem the scan found, kept as data so the table test can
// assert on it without a Makefile on disk (memql#3003).
type runFinding struct {
	lineNo  int
	pattern string
	pkgs    []string
	kind    string // "bad-regexp" | "no-tests" | "no-match" | "unresolvable"
	reason  string // why an "unresolvable" finding could not be resolved
}

// makefileRunAllowlist exempts a `-run` recipe the parser genuinely cannot
// resolve, keyed by its RAW `-run` value, with the reason it is exempt.
//
// The escape hatch is required rather than decorative: resolveMakePattern
// reports `$$IDENT` unresolvable on purpose, because a shell variable resolves
// at run time and unescaping it would report a working parameterised recipe as
// broken (#3018 landing review). Without an allow-list, making unresolvable a
// FAILURE would fail such a recipe.
//
// It is EMPTY, and that is load-bearing rather than a placeholder: an empty
// allow-list plus a passing gate is the proof that no recipe in this tree is
// currently being skipped. Adding an entry is the deliberate, reviewable act
// of removing one recipe from coverage -- which is precisely what memql#3027
// is about making impossible to do by accident.
var makefileRunAllowlist = map[string]string{}

// scanMakefileRunPatterns is the whole rule, separated from the filesystem so
// it can be driven by synthetic Makefiles.
//
// namesFor resolves package references to declared test names; the real gate
// reads them off disk, the table test supplies them directly. checked counts
// the patterns actually resolved, which is what the "this gate has stopped
// guarding anything" assertion keys on.
func scanMakefileRunPatterns(raw string, namesFor func(pkgs []string) []string) (findings []runFinding, checked int) {
	for _, ll := range foldMakeContinuations(raw) {
		if strings.HasPrefix(strings.TrimLeft(ll.text, " \t"), "#") {
			continue // prose, not a recipe
		}
		// Segment first, so a pattern is scored only against the packages of
		// its own command (memql#3003 M4).
		// memql#3027 gap 2: `cd <dir> && go test ...`. Operands resolve
		// against dir, while testNamesInPackages resolves against the gate's
		// CWD, and splitShellCommands discards the cd segment's context by
		// design. Detected across the whole logical line rather than per
		// segment, because the cd is a PRECEDING segment. The Makefile already
		// uses `cd <dir> &&` at several recipes -- they are npm today, so
		// nothing fires, which is exactly the kind of latent gap this issue is
		// about.
		sawCd := false
		for _, cmd := range splitShellCommands(ll.text) {
			if fields := strings.Fields(cmd); len(fields) > 0 &&
				strings.TrimPrefix(strings.Trim(fields[0], "'\""), "@") == "cd" {
				sawCd = true
			}

			rawPattern, ok := lastRunPattern(cmd)
			if !ok {
				continue
			}

			// Establish FIRST whether this is even a go-test invocation. A
			// help string carrying the text `-run` is not, and must stay
			// silent -- see the note in commandPackages.
			pkgs, unresolvable, isGoTest := commandPackages(cmd)
			if !isGoTest {
				continue
			}
			if reason, exempt := makefileRunAllowlist[rawPattern]; exempt {
				_ = reason
				continue
			}

			pattern, resolvable := resolveMakePattern(rawPattern)
			if !resolvable {
				findings = append(findings, runFinding{ll.lineNo, rawPattern, nil, "unresolvable",
					"the -run value cannot be resolved at this layer (a Make variable, or a shell variable resolved at run time)"})
				continue
			}
			if _, cerr := regexp.Compile(pattern); cerr != nil {
				findings = append(findings, runFinding{ll.lineNo, pattern, nil, "bad-regexp", ""})
				continue
			}
			if sawCd {
				findings = append(findings, runFinding{ll.lineNo, pattern, nil, "unresolvable",
					"a preceding `cd` changes the directory the package operands resolve against"})
				continue
			}
			if unresolvable {
				findings = append(findings, runFinding{ll.lineNo, pattern, pkgs, "unresolvable",
					"a package operand cannot be resolved at this layer (a Make or shell variable, a module path, or -C)"})
				continue
			}
			if len(pkgs) == 0 {
				pkgs = []string{"."} // go test with no package argument means the current dir
			}

			names := namesFor(pkgs)
			if len(names) == 0 {
				findings = append(findings, runFinding{ll.lineNo, pattern, pkgs, "no-tests", ""})
				continue
			}

			checked++
			if !matchesTopLevel(pattern, names) {
				findings = append(findings, runFinding{ll.lineNo, pattern, pkgs, "no-match", ""})
			}
		}
	}
	return findings, checked
}

func TestMakefileRunPatternsMatchARealTest(t *testing.T) {
	raw, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	findings, checked := scanMakefileRunPatterns(string(raw), func(pkgs []string) []string {
		return testNamesInPackages(t, pkgs)
	})

	for _, f := range findings {
		switch f.kind {
		case "bad-regexp":
			t.Errorf("Makefile:%d: -run %s is not a valid regexp", f.lineNo, f.pattern)
		case "no-tests":
			t.Errorf("Makefile:%d: -run %s targets %s, which declares no Go tests at all",
				f.lineNo, f.pattern, strings.Join(f.pkgs, " "))
		case "unresolvable":
			t.Errorf("Makefile:%d: -run %s could not be resolved by this gate, so it was NOT "+
				"checked: %s.\n"+
				"An unresolvable recipe is a silent hole in coverage -- the gate examines it, "+
				"gives up, and reports success. Either make it resolvable (name the package "+
				"literally, drop the `cd`/`-C`), or add it to makefileRunAllowlist with the "+
				"reason it cannot be checked (memql#3027).",
				f.lineNo, f.pattern, f.reason)
		case "no-match":
			names := testNamesInPackages(t, f.pkgs)
			t.Errorf("Makefile:%d: -run %s matches none of the %d tests declared in %s, "+
				"so this target exits 0 with \"[no tests to run]\" and reports success "+
				"without running anything. Declared there: %s",
				f.lineNo, f.pattern, len(names), strings.Join(f.pkgs, " "), strings.Join(names, ", "))
		}
	}

	// The global floor stays as a backstop, but it is no longer the only
	// protection: every unresolvable recipe is now its own finding above, so
	// coverage cannot drain away one recipe at a time while this stays >0
	// (memql#3027).
	if checked == 0 {
		t.Error("no resolvable -run pattern found in the Makefile; this gate has stopped " +
			"guarding anything and its parsing has probably drifted from the recipes")
	}
}

// testNamesInPackages collects every declared Go test name across the given
// package references. `./...` is expanded by walking the tree, which is what
// go test means by it.
func testNamesInPackages(t *testing.T, pkgs []string) []string {
	t.Helper()

	var names []string
	for _, p := range pkgs {
		if strings.HasSuffix(p, "/...") {
			root := strings.TrimSuffix(strings.TrimSuffix(p, "..."), "/")
			if root == "." || root == "" {
				root = "."
			}
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
					return nil
				}
				names = append(names, testNamesInFile(path)...)
				return nil
			})
			continue
		}

		entries, err := os.ReadDir(p)
		if err != nil {
			continue // an unreadable package is reported by the caller as "no tests"
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			names = append(names, testNamesInFile(filepath.Join(p, e.Name()))...)
		}
	}
	return names
}

func testNamesInFile(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	for _, m := range goTestDecl.FindAllStringSubmatch(string(b), -1) {
		names = append(names, m[1])
	}
	return names
}

// matchesTopLevel applies -run semantics against TOP-LEVEL test names: the
// pattern is split the way testing.splitRegexp splits it, and any alternative
// whose first element matches any declared name is a match (memql#3003 M3).
func matchesTopLevel(pattern string, names []string) bool {
	for _, alt := range topLevelAlternatives(pattern) {
		re, err := regexp.Compile(alt)
		if err != nil {
			// Unrepresentable at this layer -- stay permissive rather than
			// report a recipe broken on the strength of a parse we got wrong.
			return true
		}
		for _, n := range names {
			if re.MatchString(n) {
				return true
			}
		}
	}
	return false
}
