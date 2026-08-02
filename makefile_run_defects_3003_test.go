package main

import (
	"os"
	"strings"
	"testing"
)

// makefile_run_defects_3003_test.go -- memql#3003.
//
// TestMakefileRunPatternsMatchARealTest exists to catch a `go test -run`
// recipe whose pattern matches no test in its target package. As merged
// (memql#2970) it missed that class in five ways, four of them in BOTH
// directions -- a false negative lets the very defect through, a false
// positive fails a recipe that works.
//
// This guard has now been wrong twice. The third time should be caught by a
// test rather than by an audit, so every case below is pinned against a
// synthetic Makefile with a canned package resolver: no filesystem, no
// dependence on what the real tree happens to declare today.
//
// Each case is a reproduction from the issue, not an invention.

// fakePkgs is the canned resolver. `.` deliberately declares names that also
// look plausible elsewhere, because the M2 false negative turned on exactly
// that: a pattern misattributed to the root package found a real name there
// and passed.
var fakePkgs = map[string][]string{
	".":                         {"TestRunPATSubcommand_Help", "TestApplySubcommandEnv"},
	"./component/architecture/": {"TestArchitectureModelIsNotStale", "TestCommittedModelIsSorted"},
	"./pkg/":                    {"TestReal", "TestRealNested"},
	"./other/":                  {"TestOther"},
}

func fakeNamesFor(pkgs []string) []string {
	var out []string
	for _, p := range pkgs {
		out = append(out, fakePkgs[p]...)
	}
	return out
}

// scanSynthetic runs the rule over a synthetic Makefile.
func scanSynthetic(t *testing.T, makefile string) ([]runFinding, int) {
	t.Helper()
	return scanMakefileRunPatterns(makefile, fakeNamesFor)
}

func TestMakefileRunGuard_Defects(t *testing.T) {
	const arch = "./component/architecture/"

	for _, tc := range []struct {
		name        string
		makefile    string
		wantFlag    bool // the scan must report a problem
		wantChecked bool // ... and/or must have resolved at least one pattern
		why         string
	}{
		// ---- M1: `$` in a pattern ------------------------------------------
		{
			name:     "M1 end-anchored phantom is FLAGGED",
			makefile: "t:\n\t$(GO) test -run '^TestArchitectureModelIsCurrent$$' " + arch + "\n",
			wantFlag: true, wantChecked: true,
			why: "`$$` is Make's escape for a literal `$`, the only way to deliver a regexp end " +
				"anchor to the shell. Treating any `$` as a Make variable skipped the most " +
				"idiomatic spelling of memql#2923's own recipe -- the gate was blind to the " +
				"class it was written to close.",
		},
		{
			name:     "M1 end-anchored REAL name stays silent",
			makefile: "t:\n\t$(GO) test -run '^TestArchitectureModelIsNotStale$$' " + arch + "\n",
			wantFlag: false, wantChecked: true,
			why: "the mirror: unescaping `$$` must not start reporting working recipes.",
		},
		{
			name:     "M1 a genuine Make variable is still skipped",
			makefile: "t:\n\t$(GO) test -run $(PATTERN) " + arch + "\n",
			wantFlag: false, wantChecked: false,
			why: "`$(` and `${` are variables and are genuinely unresolvable at this layer; " +
				"only the `$$` escape was being conflated with them.",
		},

		// ---- M2: continued recipes ------------------------------------------
		{
			name:     "M2 continuation keeps its own package (no false positive)",
			makefile: "t:\n\t$(GO) test -run TestArchitectureModelIsNotStale \\\n\t\t" + arch + "\n",
			wantFlag: false, wantChecked: true,
			why: "without folding, the package reference is on another physical line, `pkgs` " +
				"defaults to `.`, and a working recipe is reported as matching none of the " +
				"root package's tests.",
		},
		{
			name:     "M2 continuation is still FLAGGED when genuinely broken",
			makefile: "t:\n\t$(GO) test -run TestRunPATSubcommand_Help \\\n\t\t" + arch + "\n",
			wantFlag: true, wantChecked: true,
			why: "the worse direction. That name IS declared at the root, so misattributing " +
				"the package made a genuinely broken recipe pass.",
		},

		// ---- M3: package and pattern shapes ---------------------------------
		{
			name:     "M3 Make variable in PACKAGE position is skipped",
			makefile: "t:\n\t$(GO) test -run TestReal $(ARCH_PKG)\n",
			wantFlag: false, wantChecked: false,
			why: "`$` was checked in the pattern and never in the package, so this scored " +
				"against the root package and reported `matches none of the ... tests in .`.",
		},
		{
			name:     "M3 module path in package position is skipped",
			makefile: "t:\n\t$(GO) test -run TestReal github.com/znasllc-io/memql/component/architecture\n",
			wantFlag: false, wantChecked: false,
			why: "a module path has no `./` for the old package regexp to anchor on, so it " +
				"fell through to the root default.",
		},
		{
			name:     "M3 subtest pattern is not reported broken",
			makefile: "t:\n\t$(GO) test -run TestReal/subcase ./pkg/\n",
			wantFlag: false, wantChecked: true,
			why: "`go test` splits on unbracketed `/` and matches each element at a successive " +
				"level, so compiling the whole string can never match a top-level name.",
		},
		{
			name:     "M3 top-level alternation is not reported broken",
			makefile: "t:\n\t$(GO) test -run 'Nope/x|TestReal' ./pkg/\n",
			wantFlag: false, wantChecked: true,
			why: "a top-level `|` makes the WHOLE pattern an alternation, so this genuinely " +
				"runs TestReal. Splitting on `/` first yields `Nope` and reports it broken.",
		},
		{
			name:     "M3 a slash inside a character class is not a separator",
			makefile: "t:\n\t$(GO) test -run 'TestReal[A-Z/a-z]*' ./pkg/\n",
			wantFlag: false, wantChecked: true,
			why: "splitting it yields `TestReal[A-Z`, which does not compile -- `missing " +
				"closing ]`.",
		},
		{
			name:     "M3 a genuinely absent subtest root is still FLAGGED",
			makefile: "t:\n\t$(GO) test -run TestPhantom/subcase ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "subtest handling must not become a blanket pass.",
		},

		// ---- M4: which -run wins --------------------------------------------
		{
			name:     "M4 the LAST -run on a line is the one that counts",
			makefile: "t:\n\t$(GO) test -run TestReal -run TestPhantom ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "`go test` honours the last -run; the gate read the first via " +
				"FindStringSubmatch and reported ok.",
		},
		{
			name:     "M4 a phantom in a CHAINED command is caught",
			makefile: "t:\n\t$(GO) test -run TestReal ./pkg/ && $(GO) test -run TestPhantom ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "the second half is memql#2923 verbatim. Reading one -run per line missed it, " +
				"and `checked` was still 1 so the `checked == 0` net never fired either.",
		},
		{
			name:     "M4 packages are not unioned across chained commands",
			makefile: "t:\n\t$(GO) test -run TestOther ./other/ && $(GO) test -run TestOther ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "unioning packages across the whole line let a phantom for pkg2 pass by " +
				"naming a test declared in pkg1.",
		},

		// ---- M5: flag spellings ---------------------------------------------
		{
			name:     "M5 --run is caught",
			makefile: "t:\n\t$(GO) test --run TestPhantom ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "Go's flag package accepts `--run` identically; the old anchor could not " +
				"match it because the preceding character is `-`.",
		},
		{
			name:     "M5 -test.run is caught",
			makefile: "t:\n\t$(GO) test -test.run TestPhantom ./pkg/\n",
			wantFlag: true, wantChecked: true,
			why: "same flag, third spelling.",
		},
		{
			name:     "M5 the word Re-run in prose is still not a flag",
			makefile: "## Re-run this target after editing.\nt:\n\t$(GO) test -run TestReal ./pkg/\n",
			wantFlag: false, wantChecked: true,
			why: "the leading boundary must survive the widening -- a guard that reports prose " +
				"as a broken target is noise, and noisy guards get ignored.",
		},
		{
			name:     "--dry-run is still not a -run flag",
			makefile: "t:\n\t$(GO) test -run TestReal ./pkg/ --dry-run\n",
			wantFlag: false, wantChecked: true,
			why: "measured against the real Makefile, which carries --dry-run at three sites.",
		},

		// ---- preserved behaviour --------------------------------------------
		{
			name:     "no package argument still means the current directory",
			makefile: "t:\n\t$(GO) test -run TestRunPATSubcommand_Help\n",
			wantFlag: false, wantChecked: true,
			why: "`go test -run X` with no package argument legitimately means `.`; the " +
				"default must not be removed while fixing the misattribution.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, checked := scanSynthetic(t, tc.makefile)
			if got := len(findings) > 0; got != tc.wantFlag {
				t.Errorf("flagged=%v, want %v\n  why: %s\n  findings: %+v", got, tc.wantFlag, tc.why, findings)
			}
			if got := checked > 0; got != tc.wantChecked {
				t.Errorf("checked>0 = %v, want %v\n  why: %s", got, tc.wantChecked, tc.why)
			}
		})
	}
}

// TestMakefileRunGuard_ReportsTheFirstPhysicalLine pins the line-number half of
// M2. Folding continuations with a naive ReplaceAll fixes detection and then
// misreports the location -- measured off by 70 lines in one case -- which
// turns a real finding into an unfindable one.
func TestMakefileRunGuard_ReportsTheFirstPhysicalLine(t *testing.T) {
	makefile := strings.Repeat("# filler\n", 20) +
		"target:\n" +
		"\t$(GO) test -run TestPhantom \\\n" +
		"\t\t./pkg/\n"

	findings, _ := scanSynthetic(t, makefile)
	if len(findings) != 1 {
		t.Fatalf("expected exactly one finding, got %d: %+v", len(findings), findings)
	}
	// "# filler" x20 = lines 1-20, "target:" = 21, the recipe starts on 22.
	if findings[0].lineNo != 22 {
		t.Errorf("finding reported at line %d, want 22 -- the FIRST physical line of the "+
			"folded recipe. A number that points at the wrong line is worse than no number: "+
			"it sends the reader somewhere else in the file (memql#3003 M2).", findings[0].lineNo)
	}
}

// TestMakefileRunGuard_CatchesIssue2923Verbatim is the end-to-end
// reconstruction: the exact recipe memql#2923 was filed about, against the
// real package, with the real test names read off disk.
//
// The guard is only worth having if it catches THAT. Everything above is
// synthetic; this one is not.
func TestMakefileRunGuard_CatchesIssue2923Verbatim(t *testing.T) {
	const pkg = "./component/architecture/"
	if _, err := os.Stat(strings.TrimPrefix(pkg, "./")); err != nil {
		t.Skipf("%s is not present in this checkout: %v", pkg, err)
	}

	namesFor := func(pkgs []string) []string { return testNamesInPackages(t, pkgs) }

	// #2923 verbatim: the target ran TestArchitectureModelIsCurrent, while the
	// staleness gate is really named TestArchitectureModelIsNotStale.
	broken := "arch-model-check:\n\t$(GO) test -count=1 -run TestArchitectureModelIsCurrent " + pkg + "\n"
	findings, checked := scanMakefileRunPatterns(broken, namesFor)
	if checked == 0 {
		t.Fatal("the #2923 recipe resolved no pattern at all -- the parsing has drifted and " +
			"the gate is not looking at this recipe")
	}
	if len(findings) != 1 || findings[0].kind != "no-match" {
		t.Errorf("the guard did not flag memql#2923's own recipe. That recipe exits 0 with "+
			"\"[no tests to run]\" and reports success without running anything, which is the "+
			"entire reason this gate exists.\n  findings: %+v", findings)
	}

	// And the corrected recipe, which is what main carries today, must stay silent.
	fixed := "arch-model-check:\n\t$(GO) test -count=1 -run TestArchitectureModelIsNotStale " + pkg + "\n"
	if findings, _ := scanMakefileRunPatterns(fixed, namesFor); len(findings) != 0 {
		t.Errorf("the guard flagged the CORRECTED recipe, which is the one in the Makefile "+
			"today.\n  findings: %+v", findings)
	}
}
