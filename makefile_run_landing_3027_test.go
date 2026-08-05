package main

import (
	"strings"
	"testing"
)

// makefile_run_landing_3027_test.go closes the gaps the memql#3027 landing
// review found in the first cut of that change.
//
// They divide into two families, and the split is the point. memql#3027's
// thesis is that a `-run` recipe must never drop out of coverage in silence;
// making "unresolvable" a hard failure delivers that, but it also means every
// MIS-classification now breaks the build. So the first cut had to be right in
// both directions at once, and it was not:
//
//   - SILENT: `@go test` was classified as "not a go test command" and dropped
//     with no finding and no `checked` -- the exact failure the change exists to
//     end, arriving one layer earlier than the new finding can reach, and a
//     regression against the bare-word match it replaced.
//   - LOUD BUT WRONG: a trailing `#` comment, a `2>&1` redirect and an `-args`
//     operand each marked a working recipe unresolvable. This file's own header
//     calls a gate that blocks valid work worse than the invisible target it
//     replaced.
//
// Every case below was measured against the real `go` toolchain or GNU Make
// before being asserted, because the whole gate is a claim about what the shell
// and `go test` actually do with a recipe.

// TestMakefileRunGuard_MakeRecipePrefixIsStillAGoTest pins the SILENT half.
//
// `@` silences the echo, `-` ignores errors, `+` always runs. Make glues them
// to the tool word, so `fields[0]` is `@go`, which matched none of the tool
// arms and made the whole recipe invisible. `@` is this Makefile's dominant
// idiom -- 22 of its recipe lines start with it.
//
// The `checked == 0` floor is no backstop for this: the one `$(GO)` recipe in
// the tree holds the count at 1 forever, so coverage drains one recipe at a
// time with the gate reporting success. That is precisely the "silent disable"
// memql#3027 was filed about.
func TestMakefileRunGuard_MakeRecipePrefixIsStillAGoTest(t *testing.T) {
	for name, recipe := range map[string]string{
		"at-silenced":       "\t@go test -run TestPhantomNoSuchTest ./pkg/",
		"ignore-error":      "\t-go test -run TestPhantomNoSuchTest ./pkg/",
		"always-run":        "\t+go test -run TestPhantomNoSuchTest ./pkg/",
		"at-then-ignore":    "\t@-go test -run TestPhantomNoSuchTest ./pkg/",
		"at-with-make-var":  "\t@$(GO) test -run TestPhantomNoSuchTest ./pkg/",
		"at-with-full-path": "\t@/usr/local/go/bin/go test -run TestPhantomNoSuchTest ./pkg/",
	} {
		t.Run(name, func(t *testing.T) {
			findings, checked := gapScan(t, "t:\n"+recipe+"\n")
			if checked == 0 || len(findings) == 0 {
				t.Fatalf("a Make recipe prefix made the recipe INVISIBLE to the guard.\n"+
					"  recipe: %s\n  checked=%d findings=%v\n"+
					"The pattern is a phantom, so this must be reported. A prefix is not part "+
					"of the tool name, and dropping the recipe here is the silent skip "+
					"memql#3027 exists to end -- reached before the `unresolvable` finding can "+
					"fire, and not caught by the `checked == 0` floor because another recipe "+
					"keeps the count above zero.", recipe, checked, findings)
			}
		})
	}

	// The counterpart: the prefix strip must not loosen the tool check that
	// keeps `npm test` out. Only the START of a command may carry a prefix.
	for name, recipe := range map[string]string{
		"npm":            "\t@npm test -- --run TestPhantomNoSuchTest ./pkg/",
		"cargo":          "\t@cargo test --run TestPhantomNoSuchTest",
		"not-go-suffix":  "\t@mygo test -run TestPhantomNoSuchTest ./pkg/",
		"prefix-mid-cmd": "\techo x && @npm test -- --run TestPhantomNoSuchTest ./pkg/",
	} {
		t.Run("still-silent/"+name, func(t *testing.T) {
			findings, checked := gapScan(t, "t:\n"+recipe+"\n")
			if checked != 0 || len(findings) != 0 {
				t.Fatalf("a non-go-test command was scored as a go test.\n"+
					"  recipe: %s\n  checked=%d findings=%v\n"+
					"Stripping the Make prefix must not widen the tool check -- a hard CI "+
					"failure on an npm recipe is the regression this guard already had to fix "+
					"once.", recipe, checked, findings)
			}
		})
	}
}

// TestMakefileRunGuard_TrailingCommentIsNotAnOperand pins the LOUD-BUT-WRONG
// half for shell comments.
//
// Verified against GNU Make 4.3: `#` is a Make comment only at the START of a
// line, never in a recipe, so Make passes the whole line to the shell -- which
// strips the comment, but only where `#` begins a word. Both halves of that
// rule matter here and are asserted below.
//
// memql#3027 named this shape and the first cut left it half-closed: the
// leading-`#` case was pinned, the trailing one was not. While unresolvable was
// silent it merely dropped coverage; once it became a hard failure it turned
// into a false positive on a working recipe.
func TestMakefileRunGuard_TrailingCommentIsNotAnOperand(t *testing.T) {
	t.Run("trailing comment resolves", func(t *testing.T) {
		findings, checked := gapScan(t, "t:\n\tgo test -run TestReal ./pkg/ # only the fast one\n")
		if checked != 1 || len(findings) != 0 {
			t.Fatalf("a working recipe with a trailing shell comment was reported broken.\n"+
				"  checked=%d findings=%v\n"+
				"The shell drops everything after a word-initial `#`, so this recipe runs "+
				"exactly as `go test -run TestReal ./pkg/` -- measured with GNU Make, not "+
				"assumed. Reporting it unresolvable blocks valid work, which this file's own "+
				"header calls worse than the invisible target the guard replaced.",
				checked, findings)
		}
	})

	t.Run("comment hides no phantom", func(t *testing.T) {
		// The false NEGATIVE the same defect produced: a genuinely phantom
		// pattern was reported as "unresolvable" rather than "no-match", so the
		// diagnostic pointed at the parser instead of at the broken recipe.
		findings, _ := gapScan(t, "t:\n\tgo test -run TestPhantomNoSuchTest ./pkg/ # note\n")
		if len(findings) != 1 || findings[0].kind != "no-match" {
			t.Fatalf("a phantom pattern behind a comment must still be a no-match.\n"+
				"  findings=%v\nGot kind %q; the comment must not change WHICH defect is "+
				"reported.", findings, kindOf(findings))
		}
	})

	t.Run("hash mid-word is a real operand", func(t *testing.T) {
		// `./pkg/#x` is NOT a comment -- the shell strips only a word-INITIAL
		// `#` -- so the `#` is part of the operand and the recipe genuinely
		// names a package that does not exist. It must stay loud, and the
		// operand must arrive intact rather than being truncated at the `#`.
		findings, _ := gapScan(t, "t:\n\tgo test -run TestReal ./pkg/#x\n")
		if len(findings) == 0 {
			t.Fatalf("`./pkg/#x` is a literal operand, not a comment, so this recipe is "+
				"genuinely broken and must be reported.\n  findings=%v", findings)
		}
		if got := strings.Join(findings[0].pkgs, ","); got != "./pkg/#x" {
			t.Fatalf("the `#` must stay attached to the operand -- truncating it here would "+
				"score a package the recipe never names.\n  want ./pkg/#x, got %q", got)
		}
	})

	t.Run("quoted hash is data", func(t *testing.T) {
		// The quote state has to win, or a `#` inside a -run value truncates
		// the pattern.
		findings, checked := gapScan(t, "t:\n\tgo test -run 'TestPhantomNoSuchTest#x' ./pkg/\n")
		if checked != 1 || len(findings) != 1 || findings[0].kind != "no-match" {
			t.Fatalf("a `#` inside quotes is part of the pattern, not a comment.\n"+
				"  checked=%d findings=%v", checked, findings)
		}
	})
}

// TestMakefileRunGuard_RedirectFdIsNotAnOperand pins the LOUD-BUT-WRONG half
// for redirects.
//
// The splitter breaks at `>` but had already written the file-descriptor digit
// into the command, so `go test ./pkg/ 2>&1` left a bare `2` in operand
// position, hit the default arm, and marked a working recipe unresolvable.
//
// This one matters most of the family: `2>&1 | tee` is the ordinary spelling of
// the `| tee` shape the gaps table explicitly blesses as must-resolve, so an
// author following that table got a hard CI failure. `2>/dev/null` is already
// in this Makefile's vocabulary (line 25).
func TestMakefileRunGuard_RedirectFdIsNotAnOperand(t *testing.T) {
	for name, recipe := range map[string]string{
		"stderr to stdout":  "\tgo test -run TestReal ./pkg/ 2>&1",
		"stderr to devnull": "\tgo test -run TestReal ./pkg/ 2>/dev/null",
		"fd then pipe":      "\tgo test -run TestReal ./pkg/ 2>&1 | tee out.log",
		"stdout fd":         "\tgo test -run TestReal ./pkg/ 1>out.log",
		"both streams":      "\tgo test -run TestReal ./pkg/ &> out.log",
		"redirect then fd":  "\tgo test -run TestReal ./pkg/ > out.log 2>&1",
	} {
		t.Run(name, func(t *testing.T) {
			findings, checked := gapScan(t, "t:\n"+recipe+"\n")
			if checked != 1 || len(findings) != 0 {
				t.Fatalf("a redirect made a working recipe unresolvable.\n"+
					"  recipe: %s\n  checked=%d findings=%v\n"+
					"A file-descriptor digit belongs to the REDIRECT, not to the command. "+
					"Neither the fd nor the target changes which package is tested, and the "+
					"gaps table already blesses `| tee` / `>` / `>>` as must-resolve.",
					recipe, checked, findings)
			}
		})
	}

	// An operand that merely ENDS in a digit must survive: the fd strip is
	// guarded on the digit being its own word.
	t.Run("operand ending in a digit is not an fd", func(t *testing.T) {
		// The fd strip is guarded on the digit being its own word, so a package
		// whose name merely ENDS in a digit must arrive whole. Assert the
		// operand itself: a truncated `./pkg` would still produce a finding,
		// just about a package the recipe never named.
		findings, _ := gapScan(t, "t:\n\tgo test -run TestReal ./pkg2> out.log\n")
		if len(findings) != 1 {
			t.Fatalf("expected exactly one finding for an unknown package, got %v", findings)
		}
		if got := strings.Join(findings[0].pkgs, ","); got != "./pkg2" {
			t.Fatalf("`./pkg2` is a package whose name ends in a digit, not a file-descriptor "+
				"redirect. Stripping that digit scores the wrong package.\n"+
				"  want ./pkg2, got %q", got)
		}
	})
}

// TestMakefileRunGuard_ArgsEndsTheOperandScan pins the LOUD-BUT-WRONG half for
// `-args`.
//
// Everything after `-args` belongs to the test binary, and `go test` requires
// the package operands BEFORE it -- verified against the real toolchain. So
// stopping the scan there is the toolchain's own semantics, not a heuristic.
func TestMakefileRunGuard_ArgsEndsTheOperandScan(t *testing.T) {
	for name, recipe := range map[string]string{
		"bare word after args": "\tgo test -run TestReal ./pkg/ -args foo",
		"several args":         "\tgo test -run TestReal ./pkg/ -args foo bar baz",
		"flag-looking arg":     "\tgo test -run TestReal ./pkg/ -args -v",
		"dash dash args":       "\tgo test -run TestReal ./pkg/ --args foo",
	} {
		t.Run(name, func(t *testing.T) {
			findings, checked := gapScan(t, "t:\n"+recipe+"\n")
			if checked != 1 || len(findings) != 0 {
				t.Fatalf("an argument passed through to the test binary was read as a "+
					"package.\n  recipe: %s\n  checked=%d findings=%v\n"+
					"`go test` takes its package operands before `-args`; everything after it "+
					"is the binary's.", recipe, checked, findings)
			}
		})
	}
}

// TestMakefileRunGuard_DashCAttachedValueIsAlsoLoud closes the hole in the
// guard the first cut added.
//
// `-C=dir` is valid go toolchain syntax, verified against the real `go test`.
// The attached-value short-circuit ran BEFORE the `-C` check, so the guard was
// bypassed by one character and the operands went on resolving against the
// wrong directory -- silently, exactly as before the guard existed.
//
// The space-separated case gets a RELATIVE directory here on purpose. With
// `-C dir`, `dir` falls to the default arm and is unresolvable for an unrelated
// reason, so that case passes whether or not the `-C` arm exists at all -- the
// first cut's own `-C` tests asserted the right thing for the wrong reason and
// stayed green when the arm was deleted. `./sub` parses as an ordinary relative
// package, so only a real `-C` arm can catch it.
func TestMakefileRunGuard_DashCAttachedValueIsAlsoLoud(t *testing.T) {
	for name, recipe := range map[string]string{
		"attached":          "\tgo test -C=sub -run TestReal ./pkg/",
		"attached relative": "\tgo test -C=./sub -run TestReal ./pkg/",
		"spaced relative":   "\tgo test -C ./sub -run TestReal ./pkg/",
	} {
		t.Run(name, func(t *testing.T) {
			findings, _ := gapScan(t, "t:\n"+recipe+"\n")
			if len(unresolvableFindings(findings)) == 0 {
				t.Fatalf("`-C` changes the directory every relative operand resolves "+
					"against, so the gate cannot score this from the repo root.\n"+
					"  recipe: %s\n  findings=%v\n"+
					"Both spellings must be loud; `-C=dir` is accepted by the real go "+
					"toolchain.", recipe, findings)
			}
		})
	}
}

// TestMakefileRunAllowlist_SuppressesOnlyItsOwnEntry pins the escape hatch.
//
// The allow-list is the only thing that makes "unresolvable is a hard CI
// failure" survivable, and the failure message tells the operator to reach for
// it -- yet deleting the lookup outright left the whole suite green. An
// untested escape hatch is one a refactor can silently remove or mis-key, and
// nobody finds out until somebody's recipe blocks a merge.
//
// The key is the RAW `-run` value, before unescaping. That is deliberate:
// `$$IDENT` is reported unresolvable ON PURPOSE (a shell variable resolves at
// run time), so the raw spelling is what an operator can actually copy out of
// the diagnostic and paste into the map.
func TestMakefileRunAllowlist_SuppressesOnlyItsOwnEntry(t *testing.T) {
	const recipe = "t:\n\tgo test -run $$PATTERN ./pkg/\n"

	findings, _ := gapScan(t, recipe)
	if len(unresolvableFindings(findings)) == 0 {
		t.Fatalf("precondition: a shell-variable pattern must be unresolvable, got %v", findings)
	}

	restore := makefileRunAllowlist
	t.Cleanup(func() { makefileRunAllowlist = restore })

	// An entry keyed by the RAW value suppresses it.
	makefileRunAllowlist = map[string]string{
		"$$PATTERN": "parameterised recipe; the pattern resolves at run time",
	}
	if findings, _ := gapScan(t, recipe); len(findings) != 0 {
		t.Fatalf("an allow-listed recipe must be exempt, got %v", findings)
	}

	// A non-matching key must NOT suppress, or one entry silently exempts
	// everything.
	makefileRunAllowlist = map[string]string{"$$OTHER": "unrelated"}
	if findings, _ := gapScan(t, recipe); len(unresolvableFindings(findings)) == 0 {
		t.Fatalf("an allow-list entry for a DIFFERENT pattern must not exempt this one; "+
			"the map is keyed per recipe, not a global off switch. findings=%v", findings)
	}

	// Keyed by the raw value, not the unescaped one -- `$$PATTERN` unescapes to
	// `$PATTERN`, and keying on that would not match what the operator is told
	// to paste.
	makefileRunAllowlist = map[string]string{"$PATTERN": "unescaped spelling"}
	if findings, _ := gapScan(t, recipe); len(unresolvableFindings(findings)) == 0 {
		t.Fatalf("the allow-list key is the RAW `-run` value; the unescaped spelling must "+
			"not match, or the diagnostic tells operators to write a key that does not "+
			"work. findings=%v", findings)
	}
}

// TestMakefileRunGuard_UnresolvableFindingsSayWhy asserts the KIND, which the
// inverted cases in the #3003 table cannot.
//
// That table asserts only "some finding was produced", so after the inversion
// it passes on `unresolvable`, `no-tests` or `no-match` alike -- and
// unresolvable-vs-guessed is exactly the distinction #3003 fixed. Reinstating
// the #3003 defect leaves that table green.
func TestMakefileRunGuard_UnresolvableFindingsSayWhy(t *testing.T) {
	for name, tc := range map[string]struct{ recipe, wantKind string }{
		"make var in pattern": {"\t$(GO) test -run $(PATTERN) ./component/architecture/", "unresolvable"},
		"make var in package": {"\t$(GO) test -run TestReal $(ARCH_PKG)", "unresolvable"},
		"module path":         {"\tgo test -run TestReal github.com/znasllc-io/memql/component/architecture", "unresolvable"},
		"genuine phantom":     {"\tgo test -run TestPhantomNoSuchTest ./pkg/", "no-match"},
	} {
		t.Run(name, func(t *testing.T) {
			findings, _ := gapScan(t, "t:\n"+tc.recipe+"\n")
			if len(findings) != 1 || findings[0].kind != tc.wantKind {
				t.Fatalf("wrong finding KIND.\n  recipe: %s\n  want kind %q, got %v\n"+
					"`unresolvable` means the gate declined to guess; `no-match` means it "+
					"resolved the package and the pattern names nothing. Reporting one as the "+
					"other sends the reader to the wrong place, and it is the distinction "+
					"memql#3003 was filed to fix.", tc.recipe, tc.wantKind, findings)
			}
			if tc.wantKind == "unresolvable" && strings.TrimSpace(findings[0].reason) == "" {
				t.Fatalf("an unresolvable finding must carry the reason it could not be "+
					"resolved; the operator has to decide whether to fix the recipe or "+
					"allow-list it. recipe: %s", tc.recipe)
			}
		})
	}
}

// kindOf renders the kinds present, for a diagnostic.
func kindOf(findings []runFinding) string {
	var kinds []string
	for _, f := range findings {
		kinds = append(kinds, f.kind)
	}
	return strings.Join(kinds, ",")
}
