package main

import (
	"reflect"
	"strings"
	"testing"
)

// makefile_run_parser_units_3027_test.go -- memql#3027 DoD item 3.
//
// `makefile_run_parse_test.go` carries the whole parser and, despite its name,
// contained **zero `func Test`**. Its five helpers were exercised only
// transitively, through end-to-end tables that assert on findings. That is
// enough to catch a defect that changes a verdict and blind to one that does
// not -- and the guard's whole failure mode is the one that does not: it stops
// resolving, and silence looks like success.
//
// These are direct tables over the edge cases the transitive coverage cannot
// reach: quoting, CRLF, comments, and bracket depth.

func TestFoldMakeContinuations_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		raw       string
		wantTexts []string
		wantLines []int
	}{
		"no continuation": {
			raw:       "a:\n\tgo test\n",
			wantTexts: []string{"a:", "\tgo test", ""},
			wantLines: []int{1, 2, 3},
		},
		"one continuation reports the FIRST physical line": {
			raw:       "a:\n\tgo test \\\n\t\t./pkg/\n",
			wantTexts: []string{"a:", "\tgo test  \t\t./pkg/", ""},
			wantLines: []int{1, 2, 4},
		},
		"two continuations fold into one": {
			raw:       "\tgo \\\n\ttest \\\n\t./pkg/\n",
			wantTexts: []string{"\tgo  \ttest  \t./pkg/", ""},
			wantLines: []int{1, 4},
		},
		// CRLF must behave identically. Before memql#3027 the trailing "\r"
		// defeated the HasSuffix check, nothing folded, and the gate silently
		// disabled itself on a line-ending change.
		"CRLF folds exactly like LF": {
			raw:       "a:\r\n\tgo test \\\r\n\t\t./pkg/\r\n",
			wantTexts: []string{"a:", "\tgo test  \t\t./pkg/", ""},
			wantLines: []int{1, 2, 4},
		},
		"a trailing backslash at EOF still emits its line": {
			raw:       "\tgo test \\",
			wantTexts: []string{"\tgo test  "},
			wantLines: []int{1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := foldMakeContinuations(tc.raw)
			var texts []string
			var lines []int
			for _, l := range got {
				texts = append(texts, l.text)
				lines = append(lines, l.lineNo)
			}
			if !reflect.DeepEqual(texts, tc.wantTexts) {
				t.Errorf("folded text mismatch\n  want: %q\n  got:  %q", tc.wantTexts, texts)
			}
			if !reflect.DeepEqual(lines, tc.wantLines) {
				t.Errorf("line numbers mismatch -- a finding reported against the wrong line is "+
					"an unfindable one (#3003 measured a 70-line drift)\n  want: %v\n  got:  %v",
					tc.wantLines, lines)
			}
		})
	}
}

func TestSplitShellCommands_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want []string
	}{
		"chain on &&":     {"go test ./a/ && go test ./b/", []string{"go test ./a/ ", " go test ./b/"}},
		"chain on ;":      {"go test ./a/ ; go test ./b/", []string{"go test ./a/ ", " go test ./b/"}},
		"pipe":            {"go test ./a/ | tee out.log", []string{"go test ./a/ ", " tee out.log"}},
		"logical or":      {"go test ./a/ || true", []string{"go test ./a/ ", " true"}},
		"redirect":        {"go test ./a/ > out.log", []string{"go test ./a/ ", " out.log"}},
		"append redirect": {"go test ./a/ >> out.log", []string{"go test ./a/ ", " out.log"}},

		// The load-bearing quoting cases. A shell metacharacter inside quotes
		// is DATA. The `|` case is the one that bites: a top-level alternation
		// is a legitimate -run value (topLevelAlternatives exists for it), and
		// splitting the command there severs the pattern from its own packages
		// and reports a working recipe as broken.
		"pipe inside single quotes is data": {
			"go test -run 'TestA|TestB' ./a/", []string{"go test -run 'TestA|TestB' ./a/"},
		},
		"pipe inside double quotes is data": {
			`go test -run "TestA|TestB" ./a/`, []string{`go test -run "TestA|TestB" ./a/`},
		},
		"semicolon inside quotes is data": {
			"echo 'a; b'", []string{"echo 'a; b'"},
		},
		"redirect inside quotes is data": {
			"echo 'a > b'", []string{"echo 'a > b'"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := splitShellCommands(tc.line); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("segmentation mismatch\n  line: %s\n  want: %q\n  got:  %q",
					tc.line, tc.want, got)
			}
		})
	}
}

func TestCommandPackages_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd              string
		wantPkgs         []string
		wantUnresolvable bool
		wantIsGoTest     bool
	}{
		"plain":               {"go test -run X ./pkg/", []string{"./pkg/"}, false, true},
		"make var tool":       {"$(GO) test -run X ./pkg/", []string{"./pkg/"}, false, true},
		"two packages":        {"go test -run X ./a/ ./b/", []string{"./a/", "./b/"}, false, true},
		"dot package":         {"go test -run X .", []string{"."}, false, true},
		"no package":          {"go test -run X", nil, false, true},
		"quoted shell-out":    {"bash -c 'go test -run X ./pkg/'", []string{"./pkg/"}, false, true},
		"attached flag value": {"go test -count=1 -run X ./pkg/", []string{"./pkg/"}, false, true},
		"test.run spelling":   {"go test -test.run X ./pkg/", []string{"./pkg/"}, false, true},

		// Unresolvable, but definitely a go test command -- these must be
		// reported rather than skipped (memql#3027).
		"make var package":  {"go test -run X $(PKG)", nil, true, true},
		"shell var package": {"go test -run X ./$$d/", nil, true, true},
		"module path":       {"go test -run X github.com/x/y", nil, true, true},
		"dash-C":            {"go test -C dir -run X ./pkg/", []string{"./pkg/"}, true, true},

		// NOT a go test command. This must stay distinguishable, or the gate
		// hard-fails CI on a help string.
		"echoed help": {"@echo 'pass -run TestPhantom for one test'", nil, false, false},
		"npm test":    {"npm test -- --run X", nil, false, false},
		"bare word":   {"tee out.log", nil, false, false},
		"empty":       {"", nil, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			pkgs, unresolvable, isGoTest := commandPackages(tc.cmd)
			if isGoTest != tc.wantIsGoTest {
				t.Fatalf("isGoTest=%v, want %v\n  cmd: %s\n"+
					"\"not a go test command\" and \"a go test command I cannot resolve\" are "+
					"different answers; conflating them either hard-fails CI on prose or hides "+
					"a real hole (memql#3027).", isGoTest, tc.wantIsGoTest, tc.cmd)
			}
			if unresolvable != tc.wantUnresolvable {
				t.Errorf("unresolvable=%v, want %v\n  cmd: %s", unresolvable, tc.wantUnresolvable, tc.cmd)
			}
			if !tc.wantUnresolvable && !reflect.DeepEqual(pkgs, tc.wantPkgs) {
				t.Errorf("packages mismatch\n  cmd:  %s\n  want: %q\n  got:  %q", tc.cmd, tc.wantPkgs, pkgs)
			}
		})
	}
}

func TestResolveMakePattern_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		pattern        string
		want           string
		wantResolvable bool
	}{
		"plain":                {"TestFoo", "TestFoo", true},
		"end anchor":           {"^TestFoo$$", "^TestFoo$", true},
		"both anchors":         {"^TestFoo$$|^TestBar$$", "^TestFoo$|^TestBar$", true},
		"make variable":        {"$(PATTERN)", "", false},
		"make brace variable":  {"${PATTERN}", "", false},
		"shell variable":       {"$$PAT", "", false},
		"shell brace variable": {"$${PAT}", "", false},
		"underscore shell var": {"$$_x", "", false},
		"anchor then literal":  {"TestFoo$$", "TestFoo$", true},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := resolveMakePattern(tc.pattern)
			if ok != tc.wantResolvable {
				t.Fatalf("resolvable=%v, want %v (pattern %q)", ok, tc.wantResolvable, tc.pattern)
			}
			if ok && got != tc.want {
				t.Errorf("unescaped pattern mismatch\n  want: %q\n  got:  %q", tc.want, got)
			}
		})
	}
}

func TestTopLevelAlternatives_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		pattern string
		want    []string
	}{
		"plain":                    {"TestFoo", []string{"TestFoo"}},
		"subtest keeps only root":  {"TestFoo/sub", []string{"TestFoo"}},
		"alternation splits first": {"Nope/x|TestReal", []string{"Nope", "TestReal"}},
		"slash in a character class is not a separator": {
			"TestReal[A-Z/a-z]ne", []string{"TestReal[A-Z/a-z]ne"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := topLevelAlternatives(tc.pattern); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("alternatives mismatch\n  pattern: %s\n  want: %q\n  got:  %q",
					tc.pattern, tc.want, got)
			}
		})
	}
}

func TestSplitUnbracketed_Units(t *testing.T) {
	for name, tc := range map[string]struct {
		s    string
		sep  byte
		want []string
	}{
		"no separator":                  {"abc", '|', []string{"abc"}},
		"top level":                     {"a|b|c", '|', []string{"a", "b", "c"}},
		"inside brackets":               {"a[b|c]d", '|', []string{"a[b|c]d"}},
		"inside braces":                 {"a{b|c}d", '|', []string{"a{b|c}d"}},
		"inside parens":                 {"a(b|c)d", '|', []string{"a(b|c)d"}},
		"nested":                        {"a[b(c|d)e]f|g", '|', []string{"a[b(c|d)e]f", "g"}},
		"slash separator":               {"a/b", '/', []string{"a", "b"}},
		"slash in a class":              {"a[/]b", '/', []string{"a[/]b"}},
		"unbalanced close is tolerated": {"a]b|c", '|', []string{"a]b", "c"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := splitUnbracketed(tc.s, tc.sep)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("split mismatch\n  input: %q sep %q\n  want: %q\n  got:  %q",
					tc.s, string(tc.sep), tc.want, got)
			}
		})
	}
}

// TestScanMakefileRunPatterns_CommentsAndSpacing covers the two shapes the
// issue flags as passing only by accident.
//
// A `#` recipe comment tripped the unresolvable arm rather than being
// recognised as prose; there is no comment stripping, so a differently-spaced
// comment fell into gap 1. With unresolvable now LOUD, "passes by accident"
// would become "fails by accident", so these need pinning explicitly.
func TestScanMakefileRunPatterns_CommentsAndSpacing(t *testing.T) {
	for name, makefile := range map[string]string{
		"full-line comment":           "# go test -run TestPhantom ./pkg/\n",
		"indented comment":            "\t# go test -run TestPhantom ./pkg/\n",
		"comment with leading spaces": "   #go test -run TestPhantom ./pkg/\n",
	} {
		t.Run(name, func(t *testing.T) {
			findings, _ := scanMakefileRunPatterns(makefile, fakeNamesFor)
			if len(findings) != 0 {
				t.Errorf("a COMMENT produced findings %+v.\n  line: %s\n"+
					"Prose must never fail the build -- and now that unresolvable is loud, a "+
					"comment the scanner fails to recognise turns into a hard CI failure rather "+
					"than a silent skip (memql#3027).", findings, strings.TrimSpace(makefile))
			}
		})
	}
}
