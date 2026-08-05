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
		// CRLF (memql#3027 gap 4). Splitting on "\n" alone leaves a trailing
		// "\r", so HasSuffix(line, "\\") was false on every continued recipe in
		// a CRLF file: nothing folded, each orphaned backslash made its recipe
		// unresolvable, and the whole gate fell to checked=0 / findings=[].
		// The guard turned ITSELF off on a line-ending change, silently, which
		// is the failure mode it exists to prevent.
		line = strings.TrimSuffix(line, "\r")
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

// splitShellCommands segments a recipe on `&&`, `;`, `|` and redirects.
//
// memql#3027 gap 1: a pipe or a redirect used to leave `tee`, `out.log` or `>`
// sitting in operand position, where they hit commandPackages' `default:` arm
// and wrote the whole command off as unresolvable -- silently. Neither
// changes WHICH package is tested, so the right outcome is to score the
// recipe normally, and segmenting them off is what makes that happen.
// `go test ... | tee out.log` is an ordinary Makefile shape.
//
// M4's second half. Without it the package scan unions every package named
// anywhere on the line, so a phantom pattern targeting pkg2 passes by matching
// a test declared in pkg1. Segmenting first means a pattern is only ever
// scored against the packages of its OWN command.
// isShellWordBreak reports whether c ends a shell word, so a `#` immediately
// after it starts a comment. Whitespace is the obvious case; the operators
// matter because `echo x;# go test ...` is a commented-out recipe and reading
// it as live text hard-fails CI on a command that never runs.
func isShellWordBreak(c byte) bool {
	switch c {
	case ' ', '\t', ';', '&', '|', '(':
		return true
	default:
		return false
	}
}

// trimRedirectFd removes a file-descriptor prefix that belongs to a redirect
// rather than to the command: the `2` of `2>&1`, the `12` of `12>err.log`, or
// the `&` of `&>out.log`.
//
// The whole run of digits comes off, and only when it is its own word, so a
// package whose name merely ends in a digit (`./pkg2`) survives intact.
func trimRedirectFd(seg string) string {
	n := len(seg)
	if n == 0 {
		return seg
	}
	if seg[n-1] == '&' {
		if n == 1 || seg[n-2] == ' ' || seg[n-2] == '\t' {
			return seg[:n-1]
		}
		return seg
	}
	end := n
	for end > 0 && seg[end-1] >= '0' && seg[end-1] <= '9' {
		end--
	}
	if end == n {
		return seg // no digits at all
	}
	if end == 0 || seg[end-1] == ' ' || seg[end-1] == '\t' {
		return seg[:end]
	}
	return seg
}

func splitShellCommands(line string) []string {
	var cmds []string
	cur := strings.Builder{}
	// Quote state. A shell metacharacter inside quotes is DATA, not an
	// operator -- and for this gate the case that matters is a top-level
	// alternation in a -run value: `-run 'TestA|TestB'` is one pattern, and
	// splitting the command on that `|` severs the recipe from its own
	// packages and reports a working alternation as broken. topLevelAlternatives
	// exists precisely because `|` is meaningful INSIDE a pattern (#3003 M3),
	// so the splitter has to leave it alone (memql#3027).
	var quote byte
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quote != 0 {
			cur.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			cur.WriteByte(ch)
			continue
		}
		// An unquoted `#` that STARTS a word is a shell comment, so nothing
		// after it reaches `go test`. Make hands the whole recipe line to the
		// shell (`#` is a Make comment only at the START of a line, never in a
		// recipe), which is why this belongs here rather than in the scanner's
		// leading-`#` skip.
		//
		// Word-start is the POSIX rule and the distinction is load-bearing:
		// `./pkg/#x` is a literal operand the shell does NOT strip, so the `#`
		// stays attached and the recipe is scored on the package it really
		// names, and `-run 'TestA#b'` keeps its `#` -- which the quote state
		// above already guarantees.
		//
		// A word also starts after a shell OPERATOR, not only after whitespace:
		// `echo x;# go test ...` is a commented-out recipe, verified against
		// real bash and sh. Testing whitespace alone read the comment as a live
		// command and hard-failed CI on it.
		//
		// memql#3027 named this shape and left it half-closed: the leading-`#`
		// case was pinned, the trailing one was not. While unresolvable was a
		// silent skip that merely dropped coverage; once it became a hard
		// failure it turned into a false positive on a working recipe -- and a
		// false NEGATIVE too, since `-run TestPhantom ./pkg/ # note` reported
		// "unresolvable" instead of the real "no-match".
		if ch == '#' && (i == 0 || isShellWordBreak(line[i-1])) {
			break
		}
		if ch == ';' {
			cmds = append(cmds, cur.String())
			cur.Reset()
			continue
		}
		if ch == '&' && i+1 < len(line) && line[i+1] == '&' {
			cmds = append(cmds, cur.String())
			cur.Reset()
			i++
			continue
		}
		// A pipe ends the command; `||` is a two-character operator that ends
		// it just the same, so one arm covers both.
		if ch == '|' {
			cmds = append(cmds, cur.String())
			cur.Reset()
			if i+1 < len(line) && line[i+1] == '|' {
				i++
			}
			continue
		}
		// A redirect ends the command too, and its TARGET must not be read as
		// a package operand. `>`, `>>`, the `N>` fd form and `&>` start here.
		//
		// The fd digit belongs to the REDIRECT, not to the command, and it has
		// to be taken back off: the loop has already written it to `cur`, so
		// `go test ./pkg/ 2>&1` would otherwise leave a bare `2` in operand
		// position, hit the default arm, and mark a working recipe
		// unresolvable -- which this change made a hard CI failure. The earlier
		// comment here claimed the fd form was handled; it was not, and
		// `2>&1 | tee` is the more common spelling of the `| tee` shape the
		// gaps table explicitly blesses (memql#3027).
		//
		// Guarded on the whole fd being its own word, so a genuine operand
		// ending in a digit (`./pkg2`) is never truncated. The RUN of digits is
		// taken, not one: bash accepts a multi-digit fd (`12>err.log`), and
		// removing a single character there left `1` behind and hard-failed the
		// recipe anyway.
		if ch == '>' {
			cmds = append(cmds, trimRedirectFd(cur.String()))
			cur.Reset()
			if i+1 < len(line) && line[i+1] == '>' {
				i++
			}
			continue
		}
		// A single `&` backgrounds the command, so it ends it just as `;` does.
		// `&&` is consumed above and `&>` is a redirect whose `&` belongs to the
		// operator, so both are excluded here. Without this arm a trailing `&`
		// landed in operand position and marked a working recipe unresolvable --
		// the same defect as the fd digit, one operator over.
		if ch == '&' && !(i+1 < len(line) && line[i+1] == '>') {
			cmds = append(cmds, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
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
// The pattern scan deliberately reads the WHOLE command, including anything
// after `-args`.
//
// A landing-review pass truncated it there, on the reasoning that the operand
// scan stops at `-args` so this should too. That was wrong, and measuring it
// against the real toolchain is what showed why: the test binary has no `-run`
// flag, but it does have `-test.run`, and `makeRunFlag` matches both spellings.
//
//	go test -run TestReal . -args -test.run TestNoSuchThing
//	  -> ok ... [no tests to run], exit 0
//
// That is memql#2923's silent-green failure exactly, and truncating made the
// gate blind to it -- the one direction this whole rule exists to prevent. The
// shape the truncation was meant to protect is not even a working recipe:
//
//	go test -run TestReal . -args -run TestPhantom
//	  -> flag provided but not defined: -run, exit 1
//
// so the "false positive" it removed was a loud, correct complaint about a
// recipe that fails. Taking the LAST match is right in both cases, because
// `-test.run` after `-args` is precisely the one that wins at run time.
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
func commandPackages(cmd string) (pkgs []string, unresolvable bool, isGoTest bool) {
	fields := strings.Fields(cmd)
	i := 0
	for ; i < len(fields); i++ {
		// The tool must be `go` (memql#3027). Matching the bare word `test`
		// read `npm test -- --run X` as a go-test invocation, defaulted its
		// package to the ROOT, and flagged it -- a hard CI failure on an npm
		// recipe. The Makefile convention puts npm behind `cd <dir> &&`, so
		// this is reachable text rather than a hypothetical.
		//
		// Quotes are stripped first so a quoted shell-out (`bash -c 'go test
		// ...'`) still matches on its inner `go`.
		if strings.Trim(fields[i], "'\"") != "test" || i == 0 {
			continue
		}
		prev := strings.Trim(fields[i-1], "'\"")
		// Make's recipe prefixes are glued to the tool word: `@` silences the
		// echo, `-` ignores errors, `+` always runs. They are not part of the
		// tool name, and `@` is this Makefile's dominant idiom -- 22 of its
		// recipe lines start with it.
		//
		// Without this strip `@go test` classified as NOT a go-test command and
		// was dropped silently: no finding, no `checked`. That is the very
		// failure this rule exists to end, arriving one level earlier than the
		// new `unresolvable` finding can reach, and it was a REGRESSION -- the
		// bare-word match this replaced caught it. The `checked == 0` floor is
		// no backstop either, because the one `$(GO)` recipe in this tree keeps
		// the count at 1 forever.
		//
		// Only at the START of the command, where a Make prefix is legal, so
		// the `npm test` rejection this rule exists for cannot be loosened.
		if i == 1 {
			prev = strings.TrimLeft(prev, "@-+")
		}
		// `go`, an absolute/relative path ending in `go`, or a Make variable
		// holding the go binary -- `$(GO) test` is how this Makefile spells it,
		// so rejecting a variable in TOOL position would disable the gate on
		// every real recipe. This still excludes `npm test`, which is the
		// false positive being closed.
		if prev == "go" || strings.HasSuffix(prev, "/go") ||
			strings.Contains(prev, "$(") || strings.Contains(prev, "${") {
			i++
			isGoTest = true
			break
		}
	}
	if !isGoTest {
		// NOT a `go test` command at all. This must be UNRESOLVABLE, not
		// "no packages" -- the caller turns "no packages" into {"."}, so
		// returning false here scores a stray `-run` against the ROOT package
		// and fails the build on text that is not a go-test invocation.
		//
		// Measured: `@echo 'pass --run TestPhantom for one test'` and
		// `... && echo "narrow with -run TestPhantom"` were both clean on the
		// guard this replaces and flagged by it -- a hard CI failure on a HELP
		// STRING. The widened flag pattern and the new shell-command splitter
		// each make that reachable (memql#3003 landing review).
		//
		// memql#3027 makes this answer LOAD-BEARING rather than incidental.
		// Now that an unresolvable go-test command FAILS the gate, "not a go
		// test command" and "a go test command I cannot resolve" must be
		// different answers -- conflating them turns a help string into a hard
		// CI failure, which is the regression #3003's landing review already
		// had to fix once.
		return nil, false, false
	}

	for ; i < len(fields); i++ {
		// Strip surrounding quotes before classifying. A quoted shell-out --
		// `bash -c 'go test -run X ./pkg/'` -- otherwise yields the operand
		// `./pkg/'` with the quote attached, the directory read fails, and a
		// working recipe is reported as declaring no tests at all. The guard
		// this replaces got that right by accident, because its package regexp
		// stopped at the quote.
		f := strings.Trim(fields[i], "'\"")
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "-") {
			name := strings.TrimLeft(f, "-")
			// Resolve the flag's BASE NAME before deciding anything, including
			// before the attached-value short-circuit below. Doing it the other
			// way round meant `-C=dir` returned at the `=` arm and never
			// reached the `-C` check, so the guard this change added was
			// bypassed by one character and the operands went on resolving
			// against the wrong directory exactly as before. `-C=dir` is valid
			// go toolchain syntax, verified against the real `go test`.
			base := name
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				base = name[:eq]
			}
			// `-test.run` is the same flag as `-run`; without this the value
			// is read as a package operand and the command is written off as
			// unresolvable (memql#3003 M5).
			base = strings.TrimPrefix(base, "test.")
			// Everything after `-args` belongs to the TEST BINARY, not to
			// `go test`, and go requires the package operands before it. So the
			// operand scan must stop here: reading `-args foo` as a package
			// marked a working recipe unresolvable, which this change made a
			// hard CI failure.
			if base == "args" {
				return pkgs, unresolvable, true
			}
			if eq := strings.IndexByte(name, '='); eq >= 0 && base != "C" {
				continue // value is attached; nothing to skip
			}
			name = base
			if name == "C" {
				// `go test -C dir` runs in dir, so every relative operand
				// resolves against IT, not against the gate's CWD. Consuming
				// the value and then scoring `./pkg/` from the repo root reads
				// the wrong directory. Unresolvable at this layer, and now
				// LOUDLY so (memql#3027 gap 1) -- same reasoning as `cd`.
				unresolvable = true
				// Only the space-separated spelling has a separate value field
				// to skip; `-C=dir` carries its own.
				if !strings.Contains(f, "=") {
					i++
				}
				continue
			}
			if goTestValueFlags[name] {
				i++ // the next field is this flag's value, not a package
			}
			continue
		}
		switch {
		case strings.Contains(f, "$("), strings.Contains(f, "${"):
			unresolvable = true
		case strings.Contains(f, "$$"):
			// The Make dir-loop idiom in PACKAGE position -- `./$$d/`
			// (memql#3027 gap 3). `$$` is Make's escape, so the shell sees
			// `$d`: a variable resolved at run time, not a directory that
			// exists now. This arm has to precede the `./` one, or the operand
			// looks like an ordinary relative package and the recipe is
			// reported as declaring no tests -- a false positive on a working
			// recipe. resolveMakePattern already knew this about the PATTERN
			// position (#3003 M1); the operand scan never learned it.
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
	return pkgs, unresolvable, true
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
