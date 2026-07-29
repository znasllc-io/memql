package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// -run is only a flag when it stands alone. Anchoring on the preceding
	// whitespace boundary is what keeps the word "Re-run" in Makefile prose
	// (:354 today) from being read as a broken target.
	makeRunFlag = regexp.MustCompile(`(?:^|\s)-run[\s=]+['"]?([^\s'"]+)`)
	makePkgRef  = regexp.MustCompile(`\./[\w./-]*`)
	goTestDecl  = regexp.MustCompile(`(?m)^func (Test\w+)\s*\(`)
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
func TestMakefileRunPatternsMatchARealTest(t *testing.T) {
	raw, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	checked := 0
	for i, line := range strings.Split(string(raw), "\n") {
		lineNo := i + 1
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue // prose, not a recipe
		}
		m := makeRunFlag.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pattern := m[1]
		if strings.Contains(pattern, "$") {
			continue // a Make variable; nothing to resolve at this layer
		}

		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Errorf("Makefile:%d: -run %s is not a valid regexp: %v", lineNo, pattern, err)
			continue
		}

		pkgs := makePkgRef.FindAllString(line, -1)
		if len(pkgs) == 0 {
			pkgs = []string{"."} // go test with no package argument means the current dir
		}

		names := testNamesInPackages(t, pkgs)
		if len(names) == 0 {
			t.Errorf("Makefile:%d: -run %s targets %s, which declares no Go tests at all",
				lineNo, pattern, strings.Join(pkgs, " "))
			continue
		}

		checked++
		if !matchesAny(re, names) {
			t.Errorf("Makefile:%d: -run %s matches none of the %d tests declared in %s, "+
				"so this target exits 0 with \"[no tests to run]\" and reports success "+
				"without running anything. Declared there: %s",
				lineNo, pattern, len(names), strings.Join(pkgs, " "), strings.Join(names, ", "))
		}
	}

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

// matchesAny applies -run semantics: the pattern is an unanchored regexp
// tested against each test name.
func matchesAny(re *regexp.Regexp, names []string) bool {
	for _, n := range names {
		if re.MatchString(n) {
			return true
		}
	}
	return false
}
