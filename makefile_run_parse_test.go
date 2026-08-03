package main

import (
	"regexp"
	"strings"
)

// makefile_run_parse_test.go -- the parsing half of
// TestMakefileRunPatternsMatchARealTest, split out of the gate itself so each
// piece can be table-tested against synthetic input without a Makefile on disk.
//
// memql#3003. The gate as originally merged (memql#2970) got five things wrong,
// four of them in BOTH directions. Every rule below is written against a
// measured reproduction rather than a reading of the docs; the issue carries
// the reproductions and this file carries the fixes.

var (
	// M5: `go test` uses the flag package, which accepts `-run`, `--run`,
	// `-test.run` and `--test.run` identically -- measured, `--run TestPhantom`
	// exits 0 with "[no tests to run]" exactly as `-run` does. The original
	// `(?:^|\s)-run` could not match `--run`, because the character before
	// `run` is `-` rather than whitespace.
	//
	// The leading boundary stays. It is what keeps the word "Re-run" in
	// Makefile prose from being read as a flag, and differential-testing the
	// widened pattern over the real Makefile returns byte-identical results:
	// only the arch-model-check recipe matches, while `Re-run` and `--dry-run`
	// stay unmatched.
	makeRunFlag = regexp.MustCompile(`(?:^|\s)--?(?:test\.)?run[\s=]+['"]?([^\s'"]+)`)

	goTestDecl = regexp.MustCompile(`(?m)^func (Test\w+)\s*\(`)
)

// goTestValueFlags are the `go test` flags that take their value as the NEXT
// argument rather than after an `=`. Needed so the operand scan does not read
// a flag's value as a package: in `-run TestX ./pkg/`, `TestX` is not a
// package and `./pkg/` is.
var goTestValueFlags = map[string]bool{
	"run": true, "bench": true, "count": true, "timeout": true, "tags": true,
	"cpu": true, "parallel": true, "coverprofile": true, "cpuprofile": true,
	"memprofile": true, "blockprofile": true, "mutexprofile": true, "trace": true,
	"outputdir": true, "o": true, "exec": true, "gcflags": true, "ldflags": true,
	"covermode": true, "benchtime": true, "fuzztime": true, "fuzzminimizetime": true,
	"shuffle": true, "list": true,
}

// logicalLine is one Make recipe after continuation folding, carrying the
// number of the FIRST physical line it came from.
type logicalLine struct {
	text   string
	lineNo int
}

// foldMakeContinuations joins each `\`-terminated line onto the next.
//
// M2. The gate used to scan physical lines, so a recipe split across two of
// them lost its package reference: `makePkgRef` found nothing, the package
// silently defaulted to ".", and the pattern was scored against the ROOT
// package's tests. Measured in both directions against the real recipe -- a
// working recipe reported "matches none of the 17 tests declared in ." while
// blaming the wrong package, and a genuinely broken one PASSED because the
// name it references happens to be declared at the root.
//
// The line number is carried rather than recomputed. Folding with a naive
// strings.ReplaceAll fixes detection and then misreports the location -- by 70
// lines in one measured case -- which turns a real finding into an unfindable
// one.
func foldMakeContinuations(raw string) []logicalLine {
	var out []logicalLine
	var buf strings.Builder
	start := 0

	for i, line := range strings.Split(raw, "\n") {
		if buf.Len() == 0 {
			start = i + 1
		}
		if strings.HasSuffix(line, "\\") {
			buf.WriteString(strings.TrimSuffix(line, "\\"))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(line)
		out = append(out, logicalLine{text: buf.String(), lineNo: start})
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, logicalLine{text: buf.String(), lineNo: start})
	}
	return out
}

// splitShellCommands segments a recipe on `&&` and `;`.
//
// M4's second half. Without it the package scan unions every package named
// anywhere on the line, so a phantom pattern targeting pkg2 passes by matching
// a test declared in pkg1. Segmenting first means a pattern is only ever
// scored against the packages of its OWN command.
func splitShellCommands(line string) []string {
	var cmds []string
	cur := strings.Builder{}
	for i := 0; i < len(line); i++ {
		if line[i] == ';' {
			cmds = append(cmds, cur.String())
			cur.Reset()
			continue
		}
		if line[i] == '&' && i+1 < len(line) && line[i+1] == '&' {
			cmds = append(cmds, cur.String())
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(line[i])
	}
	cmds = append(cmds, cur.String())
	return cmds
}

// lastRunPattern returns the pattern `go test` would actually honour in this
// command, and whether one is present.
//
// M4's first half. The gate read the FIRST `-run` via FindStringSubmatch and
// moved on. `go test` honours the LAST: measured with `-run TestBogus -run
// TestReal`, which runs TestReal, ruling out both-applied and intersection
// semantics. So a recipe could carry a valid pattern followed by a phantom one
// and the gate reported ok.
func lastRunPattern(cmd string) (string, bool) {
	all := makeRunFlag.FindAllStringSubmatch(cmd, -1)
	if len(all) == 0 {
		return "", false
	}
	return all[len(all)-1][1], true
}

// resolveMakePattern applies Make's own escaping to a `-run` value and reports
// whether what is left can be resolved at this layer.
//
// M1, the defect that made the gate blind to the very class it was written to
// close. The old test was `strings.Contains(pattern, "$")`, justified as "a
// Make variable". A Make variable is `$(` or `${`; `$$` is Make's escape for a
// LITERAL `$`, and a literal `$` is the only way to deliver a regexp end
// anchor to the shell. So `-run '^TestFoo$$'` -- the standard Go idiom, and
// the most idiomatic spelling of the recipe memql#2923 was filed about -- was
// skipped outright.
//
// The mirror is a false positive: a Makefile whose `-run` patterns all contain
// `$` resolves none of them, and the gate's own `checked == 0` assertion then
// fails the build with "this gate has stopped guarding anything".
// A `$$` is NOT always a literal dollar either. Make passes `$$` to the shell
// as a single `$`, so `$$PAT` is a SHELL variable reference -- and unescaping
// it to `$PAT` yields an end-anchor followed by literals, which matches
// nothing, so the gate reports a working parameterised recipe as broken. That
// is the false positive this gate's own header calls worse than the invisible
// target it replaces (memql#3003 landing review), so a `$$` that introduces an
// identifier is unresolvable rather than literal.
func resolveMakePattern(pattern string) (string, bool) {
	if strings.Contains(pattern, "$(") || strings.Contains(pattern, "${") {
		return "", false // a Make variable; nothing to resolve at this layer
	}
	for i := 0; i+2 < len(pattern); i++ {
		if pattern[i] != '$' || pattern[i+1] != '$' {
			continue
		}
		if c := pattern[i+2]; c == '_' || c == '{' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return "", false // `$$IDENT` -- a shell variable, resolved at run time
		}
	}
	return strings.ReplaceAll(pattern, "$$", "$"), true
}

// commandPackages returns the package operands of a `go test` command, and
// whether an operand is present that this layer cannot resolve.
//
// M3, items 1 and 2. The old scan was `\./[\w./-]*` over the whole line, which
// silently ignored two legal spellings and scored them against the root
// package: `$(ARCH_PKG)` (a Make variable in PACKAGE position -- note the old
// code checked for `$` in the pattern but never in the package) and a full
// module path like `github.com/znasllc-io/memql/component/architecture`, which
// has no `./` for the regexp to anchor on.
//
// "No operand at all" and "an operand this layer cannot resolve" are different
// answers and must stay different. `go test -run X` with no package argument
// legitimately means the current directory, so the `{"."}` default is correct
// and is deliberately kept.
func commandPackages(cmd string) (pkgs []string, unresolvable bool) {
	fields := strings.Fields(cmd)
	i := 0
	for ; i < len(fields); i++ {
		if fields[i] == "test" {
			i++
			break
		}
	}
	if i >= len(fields) {
		return nil, false // not a `go test` command shape we understand
	}

	for ; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "-") {
			name := strings.TrimLeft(f, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue // value is attached; nothing to skip
			}
			// `-test.run` is the same flag as `-run`; without this the value
			// is read as a package operand and the command is written off as
			// unresolvable (memql#3003 M5).
			name = strings.TrimPrefix(name, "test.")
			if goTestValueFlags[name] {
				i++ // the next field is this flag's value, not a package
			}
			continue
		}
		switch {
		case strings.Contains(f, "$("), strings.Contains(f, "${"):
			unresolvable = true
		case strings.HasPrefix(f, "./"), f == ".":
			pkgs = append(pkgs, f)
		default:
			// A module path, or a bare relative name. Either may be valid; this
			// layer cannot map it to a directory, and guessing produced the
			// "matches none of the 17 tests declared in ." false positive.
			unresolvable = true
		}
	}
	return pkgs, unresolvable
}

// topLevelAlternatives returns the sub-patterns `go test` matches against a
// TOP-LEVEL test name.
//
// M3 item 3. `go test` splits a -run pattern on unbracketed `/` and matches
// each element at a successive nesting level ($GOROOT/src/testing/match.go,
// splitRegexp), so compiling the whole of `TestX/subcase` and testing it
// against a top-level name can never match -- a top-level name contains no
// slash. The gate reported every subtest recipe as broken.
//
// Two traps, both measured, both of which a literal SplitN(pattern,"/",2)[0]
// falls into:
//
//   - A top-level `|` makes the WHOLE pattern an alternation, so
//     `Nope/x|TestReal` genuinely runs TestReal. Splitting on `/` first yields
//     `Nope` and reports a working recipe as broken. Alternatives are split
//     FIRST, and any one of them matching is enough.
//   - A `/` inside a character class is not a separator: splitting
//     `TestReal[A-Z/a-z]ne` gives `TestReal[A-Z` and fails to compile with
//     "missing closing ]".
//
// Hence bracket-aware splitting, plus a compile check: an element that does
// not compile is discarded in favour of the whole alternative, so a shape this
// function models wrongly degrades to the old behaviour rather than to a false
// report.
func topLevelAlternatives(pattern string) []string {
	var out []string
	for _, alt := range splitUnbracketed(pattern, '|') {
		head := splitUnbracketed(alt, '/')[0]
		if head == "" {
			head = alt
		}
		if _, err := regexp.Compile(head); err != nil {
			head = alt
		}
		out = append(out, head)
	}
	return out
}

// splitUnbracketed splits s on sep, ignoring separators inside `[...]`,
// `(...)` or `{...}`. Mirrors the bracket tracking testing.splitRegexp does.
func splitUnbracketed(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	depthParen, depthBrace := 0, 0
	inClass := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		}
		switch {
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '(':
			depthParen++
		case c == ')':
			if depthParen > 0 {
				depthParen--
			}
		case c == '{':
			depthBrace++
		case c == '}':
			if depthBrace > 0 {
				depthBrace--
			}
		case c == sep && depthParen == 0 && depthBrace == 0:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}
