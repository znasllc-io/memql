package main

import (
	"strings"
	"testing"
)

// makefile_run_gaps_3027_test.go -- memql#3027.
//
// The residual gaps in the `-run` guard after #3003 and its landing review.
// Every one is the same failure: **the guard stops checking and says nothing.**
//
// That is worse than an ordinary gap, because silence is the exact failure mode
// the guard exists to prevent. Its only protection against being silently
// disabled was a single GLOBAL `checked == 0` assertion, so as long as one
// recipe still resolved, every other recipe could drop out of coverage
// unnoticed. Today `arch-model-check` is the only `-run` recipe in the
// Makefile, so the net looks binding -- it stops being binding the moment a
// second one is added.
//
// The fix is per-recipe accounting: a `-run` recipe the parser cannot resolve
// is a FINDING, not a skip. That converts every shape below from silent to
// loud at once, including shapes nobody has enumerated yet, which is why it
// comes first and the individual shapes are follow-through.

// gapScan drives the scanner over a synthetic Makefile with a fixed name set.
func gapScan(t *testing.T, makefile string) ([]runFinding, int) {
	t.Helper()
	return scanMakefileRunPatterns(makefile, fakeNamesFor)
}

// unresolvableFindings returns only the "cannot resolve" findings.
func unresolvableFindings(findings []runFinding) []runFinding {
	var out []runFinding
	for _, f := range findings {
		if f.kind == "unresolvable" {
			out = append(out, f)
		}
	}
	return out
}

// TestMakefileRunGuard_UnresolvableRecipeIsLoud is DoD item 1, and the change
// triage asked to be built first.
//
// Each of these is a `go test -run` recipe the parser cannot fully resolve.
// Before this change every one returned `checked=0, findings=[]` -- the guard
// examined the recipe, gave up, and reported success.
func TestMakefileRunGuard_UnresolvableRecipeIsLoud(t *testing.T) {
	for name, recipe := range map[string]string{
		// Gap 1, the half that genuinely cannot be resolved here: -C changes
		// the directory the operands resolve against, exactly like cd.
		"dash-C": "\tgo test -C dir -run TestReal ./pkg/",
		// Gap 2: cd changes the directory the operands resolve against.
		"cd then test": "\tcd component/architecture && go test -run TestReal ./",
		// Gap 3: the Make dir-loop idiom in PACKAGE position.
		"make dir loop": "\tgo test -run TestReal ./$$d/",
		// A Make variable in package position -- already unresolvable, but it
		// was silent; it must now be loud like the rest.
		"make var pkg": "\tgo test -run TestReal $(ARCH_PKG)",
	} {
		t.Run(name, func(t *testing.T) {
			findings, _ := gapScan(t, recipe)
			if len(unresolvableFindings(findings)) == 0 {
				t.Fatalf("the guard silently skipped a `-run` recipe it could not resolve, and "+
					"reported success.\n  recipe: %s\n"+
					"A recipe the parser cannot resolve must FAIL the gate (or be explicitly "+
					"allow-listed with a reason). Skipping it silently is how coverage drains "+
					"away one recipe at a time while the gate still looks green (memql#3027).",
					strings.TrimSpace(recipe))
			}
		})
	}
}

// TestMakefileRunGuard_StillSilentOnNonGoTestText is the counterpart, and the
// regression this change is most likely to cause.
//
// `commandPackages` reports "unresolvable" for TWO different things: a segment
// that is not a `go test` command at all, and a real `go test` whose operand
// cannot be resolved. While both were skipped silently the conflation was
// harmless. Making unresolvable a FAILURE separates them or the gate
// hard-fails CI on a HELP STRING -- measured and fixed once already in #3003's
// landing review, and this file's own header calls a gate that blocks valid
// work worse than the invisible target it replaced.
func TestMakefileRunGuard_StillSilentOnNonGoTestText(t *testing.T) {
	for name, recipe := range map[string]string{
		"echoed help":       "\t@echo 'pass --run TestPhantom for one test'",
		"echoed in a chain": "\t@echo \"narrow with -run TestPhantom\"",
		"comment":           "# run with -run TestPhantom",
		"non-go tool":       "\tnpm test -- --run TestPhantom",
	} {
		t.Run(name, func(t *testing.T) {
			findings, _ := gapScan(t, recipe)
			if len(findings) != 0 {
				t.Fatalf("the guard flagged text that is not a `go test` invocation: %+v\n"+
					"  recipe: %s\n"+
					"A hard CI failure on a help string is the regression memql#3003's landing "+
					"review already had to fix once. \"Not a go test command\" and \"a go test "+
					"command I cannot resolve\" must stay different answers (memql#3027).",
					findings, strings.TrimSpace(recipe))
			}
		})
	}
}

// TestMakefileRunGuard_ResolvableShapesStayResolvable guards the other
// direction: the shapes that CAN be resolved must not become collateral
// damage of the stricter accounting.
func TestMakefileRunGuard_ResolvableShapesStayResolvable(t *testing.T) {
	for name, recipe := range map[string]string{
		"plain":             "\tgo test -run TestReal ./pkg/",
		"anchored":          "\tgo test -run '^TestReal$$' ./pkg/",
		"quoted shell-out":  "\tbash -c 'go test -run TestReal ./pkg/'",
		"test.run spelling": "\tgo test -test.run TestReal ./pkg/",
		// Gap 1, the half that DOES resolve: neither a pipe nor a redirect
		// changes which package is tested, so the right outcome is to score
		// the recipe, not to shout about it. Piping test output through tee is
		// an ordinary Makefile shape.
		"pipe to tee": "\tgo test -run TestReal ./pkg/ | tee out.log",
		"redirect":    "\tgo test -run TestReal ./pkg/ > out.log",
		"append":      "\tgo test -run TestReal ./pkg/ >> out.log",
	} {
		t.Run(name, func(t *testing.T) {
			findings, checked := gapScan(t, recipe)
			if len(findings) != 0 {
				t.Errorf("a resolvable recipe was flagged: %+v\n  recipe: %s",
					findings, strings.TrimSpace(recipe))
			}
			if checked == 0 {
				t.Errorf("a resolvable recipe was not counted as checked.\n  recipe: %s\n"+
					"If it resolves it must be scored; counting it as unchecked is the silent "+
					"drain memql#3027 is about.", strings.TrimSpace(recipe))
			}
		})
	}
}

// TestFoldMakeContinuations_HandlesCRLF is gap 4.
//
// `foldMakeContinuations` split on "\n" only and then tested HasSuffix(line,
// "\\"), so on a CRLF file the trailing "\r" meant nothing folded: every
// continued recipe kept an orphaned backslash, became unresolvable, and the
// whole gate went to `checked=0, findings=[]`. The guard turned itself off on
// a line-ending change.
func TestFoldMakeContinuations_HandlesCRLF(t *testing.T) {
	const lf = "target:\n\tgo test -run TestReal \\\n\t\t./pkg/\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	lfLines := foldMakeContinuations(lf)
	crlfLines := foldMakeContinuations(crlf)

	findFolded := func(lines []logicalLine) (logicalLine, bool) {
		for _, l := range lines {
			if strings.Contains(l.text, "-run") {
				return l, true
			}
		}
		return logicalLine{}, false
	}

	lfRecipe, ok := findFolded(lfLines)
	if !ok {
		t.Fatal("the LF fixture itself did not produce a -run line; the test is wrong")
	}
	crlfRecipe, ok := findFolded(crlfLines)
	if !ok {
		t.Fatal("no -run line survived CRLF folding at all")
	}

	if strings.Contains(crlfRecipe.text, "\\") {
		t.Errorf("the continuation was not folded under CRLF -- an orphaned backslash is left, "+
			"which makes the recipe unresolvable and (before memql#3027) silently disabled the "+
			"whole gate.\n  got: %q", crlfRecipe.text)
	}
	if strings.Contains(crlfRecipe.text, "\r") {
		t.Errorf("a stray CR survived into the folded text, which corrupts every downstream "+
			"field split.\n  got: %q", crlfRecipe.text)
	}
	if !strings.Contains(crlfRecipe.text, "./pkg/") {
		t.Errorf("the continued package operand was lost under CRLF.\n  got: %q", crlfRecipe.text)
	}
	if lfRecipe.lineNo != crlfRecipe.lineNo {
		t.Errorf("line numbers diverge between LF and CRLF (%d vs %d); a finding that reports "+
			"the wrong line is an unfindable one", lfRecipe.lineNo, crlfRecipe.lineNo)
	}

	// And end to end: the CRLF Makefile must be scored exactly like the LF one.
	_, lfChecked := gapScan(t, lf)
	_, crlfChecked := gapScan(t, crlf)
	if lfChecked != crlfChecked {
		t.Errorf("CRLF changed how many recipes were checked (LF %d, CRLF %d) -- a line-ending "+
			"change must not alter what the gate guards (memql#3027)", lfChecked, crlfChecked)
	}
}
