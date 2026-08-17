package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDocumentedTestCommandCoversTheEngine fails the build when CLAUDE.md tells
// a contributor to verify with a command that does not compile the engine
// (memql#4032).
//
// # The defect this closes
//
// CLAUDE.md said "go test ./..." in three places. This is a multi-module
// workspace -- go.work lists 49 modules -- and a relative pattern resolves
// inside whichever module owns the directory it is rooted at, so `./...` never
// leaves the root module. Measured at the time of the fix: 64 packages, of which
// `component/memql`, `component/database` and `component/language` -- the engine,
// the executor, the row-authz gates and the DSL loader -- are none.
//
// The failure mode is silent and confidence-INCREASING. Someone edits
// component/memql/executor.go, runs the documented command, sees `ok` across 64
// packages, and reports the change verified. Nothing in the output hints that the
// package they edited was never built. It is worse for agent-driven work, where
// "run the documented command, paste the output" is a natural verification
// instruction taken straight from this file -- which made every such claim about
// an engine change unfounded.
//
// # Why a gate and not just a doc fix
//
// memql#4032 asked whether any lane asserted that the documented command covers
// what it claims, and named that absence as the reason this survived. `./...` is
// the form every Go developer types by reflex, so prose alone reverts on the
// first edit by someone who has not read this comment. The Makefile has been
// right since memql#3165 and the doc drifted away from it anyway; nothing
// connected the two.
//
// # It RE-DERIVES, it does not pattern-match
//
// The check runs `go list` on the patterns the document actually names and looks
// at what comes back. A string comparison against a blessed command would pass
// for a command that had stopped working, which is the whole shape of the bug.
//
// # The vacuity guard is load-bearing
//
// Two ways this could pass while measuring nothing, both defended below: finding
// no commands at all (a heading rename, a fence that stops being ```bash), and
// `go list` returning a short list for its own reasons. requireEngineHazardIsReal
// is the positive control -- it asserts `./...` STILL misses the engine, so if
// Go ever changes such that relative patterns cross module boundaries, this test
// fails and says so rather than continuing to police a hazard that no longer
// exists.
func TestDocumentedTestCommandCoversTheEngine(t *testing.T) {
	requireEngineHazardIsReal(t)

	md, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}

	cmds := documentedTestCommands(string(md))
	if len(cmds) == 0 {
		t.Fatal("found NO test command in CLAUDE.md. Either the document stopped telling " +
			"contributors how to run the tests, or this gate stopped recognising the form it " +
			"uses -- and a gate that examines nothing passes forever. Teach " +
			"documentedTestCommands the new form.")
	}

	checked := 0
	for _, cmd := range cmds {
		patterns, ok := resolvePatterns(t, cmd.text)
		if !ok {
			continue
		}
		// Only commands that CLAIM the whole tree are held to covering it. A
		// deliberately narrowed pattern (`./component/memql/...`) is honest about
		// its scope and is how the db-gated lanes are legitimately documented;
		// the defect was a pattern that LOOKS universal and is not.
		if !claimsWholeTree(patterns) {
			continue
		}
		checked++
		pkgs := goList(t, patterns)
		for _, want := range engineCorePackages {
			if !pkgs[want] {
				t.Errorf(""+
					"CLAUDE.md:%d documents `%s` as the way to run the tests, but it does not "+
					"cover %s.\n"+
					"  resolved patterns : %s\n"+
					"  packages returned : %d\n"+
					"This is a multi-module workspace, so a relative pattern like ./... resolves "+
					"inside the root module only and never reaches the engine. Someone following "+
					"this instruction gets `ok` for packages that were never compiled.\n"+
					"Use the MODULE PATH pattern, which is prefix-matched across every workspace "+
					"module -- that is what Makefile's ALL_PKGS is (memql#3165) and what `make "+
					"test` runs.\n"+
					"(If you are reading this after running with GOWORK=off, that is the cause "+
					"rather than the document: outside workspace mode the module pattern reaches "+
					"one module too. CI always runs this in workspace mode.)",
					cmd.line, cmd.text, want, strings.Join(patterns, " "), len(pkgs))
				break
			}
		}
	}

	if checked == 0 {
		t.Fatalf("CLAUDE.md names %d test command(s), but NONE of them claims to run the whole "+
			"tree, so this gate verified nothing. The document has to keep telling contributors "+
			"how to verify a change across every module -- that instruction is the thing "+
			"memql#4032 was about. Commands found: %s", len(cmds), summarise(cmds))
	}
}

// claimsWholeTree reports whether these patterns present themselves as "every
// package", which is the claim that has to be true rather than merely typed.
//
// `./...` qualifies precisely because it is the trap: it reads as universal and
// resolves inside one module. The Makefile's own selector qualifies because that
// is what `make test` expands to. A subtree pattern qualifies as neither.
func claimsWholeTree(patterns []string) bool {
	moduleWide, _ := makefileAllPkgs()
	for _, p := range patterns {
		if p == "./..." || p == "all" {
			return true
		}
		if moduleWide != "" && p == moduleWide {
			return true
		}
	}
	return false
}

func summarise(cmds []documentedCommand) string {
	var parts []string
	for _, c := range cmds {
		parts = append(parts, "CLAUDE.md:"+strconv.Itoa(c.line)+" `"+c.text+"`")
	}
	return strings.Join(parts, "; ")
}

// engineCorePackages are packages whose absence makes a green meaningless: the
// engine and executor, the storage layer, and the DSL loader. Each lives in its
// own module, which is exactly why `./...` cannot see them.
var engineCorePackages = []string{
	"github.com/znasllc-io/memql/component/memql",
	"github.com/znasllc-io/memql/component/database",
	"github.com/znasllc-io/memql/component/language",
}

// requireEngineHazardIsReal is the POSITIVE CONTROL.
//
// It asserts the thing this gate exists to catch is still catchable: that
// `go list ./...` really does miss the engine. If that stops being true -- a
// toolchain change, or the modules being folded back into one -- the assertion
// below would pass for a reason unrelated to the document being correct, and
// this gate would be policing nothing. Failing loudly is the honest outcome:
// somebody should then decide whether the warning in CLAUDE.md is still owed.
func requireEngineHazardIsReal(t *testing.T) {
	t.Helper()
	pkgs := goList(t, []string{"./..."})
	if len(pkgs) == 0 {
		t.Fatal("`go list ./...` returned nothing; this gate cannot establish its own premise")
	}
	for _, p := range engineCorePackages {
		if pkgs[p] {
			t.Fatalf("`go list ./...` now reaches %s (%d packages), so the multi-module hazard "+
				"this gate polices no longer exists. Re-check CLAUDE.md's Testing section: the "+
				"warning it carries may now be wrong, which is worse than absent.", p, len(pkgs))
		}
	}
}

type documentedCommand struct {
	text string
	line int
}

// goTestLine matches a `go test`/`make test` invocation wherever CLAUDE.md
// presents one: on its own inside a fenced block, or inside backticks in a
// table cell or a sentence. Leading `VAR=value` assignments are tolerated
// because the document uses them (MEMQL_REQUIRE_DB=1).
var goTestLine = regexp.MustCompile(`(?:^|` + "`" + `)((?:[A-Z][A-Z0-9_]*=\S+\s+)*(?:go test|make test)\b[^` + "`" + `\n]*)`)

// documentedTestCommands pulls every command CLAUDE.md presents as a way to run
// the tests.
//
// Lines that are explicitly telling the reader NOT to use a form are skipped --
// the whole point of the Testing section is to name the wrong command and say
// why, and a gate that then failed on that sentence would make the warning
// unwritable.
func documentedTestCommands(md string) []documentedCommand {
	var out []documentedCommand
	for i, raw := range strings.Split(md, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isCounterExample(line) {
			continue
		}
		for _, m := range goTestLine.FindAllStringSubmatch(raw, -1) {
			cmd := strings.TrimSpace(m[1])
			// Trailing prose after a table-cell command, and inline comments.
			if idx := strings.Index(cmd, " #"); idx >= 0 {
				cmd = strings.TrimSpace(cmd[:idx])
			}
			out = append(out, documentedCommand{text: cmd, line: i + 1})
		}
	}
	return out
}

// isCounterExample reports whether a line is naming a command in order to warn
// against it rather than to recommend it.
func isCounterExample(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{
		"do not", "don't", "not `go test", "rather than", "instead of",
		"misses", "does not run", "does not cover", "not the way",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// Table rows comparing commands (the measured table in Testing) carry their
	// verdict in a later cell.
	return strings.HasPrefix(line, "|") && (strings.Contains(lower, "**no**") || strings.Contains(lower, "| no |"))
}

// resolvePatterns turns a documented command into the package patterns it would
// hand `go list`. `make test` is resolved through the Makefile so the gate
// follows the same indirection a contributor does; a command naming no patterns
// at all is reported rather than skipped.
func resolvePatterns(t *testing.T, cmd string) ([]string, bool) {
	t.Helper()
	fields := strings.Fields(cmd)

	// Drop leading environment assignments.
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return nil, false
	}

	if fields[0] == "make" {
		pat, err := makefileAllPkgs()
		if err != nil {
			t.Errorf("CLAUDE.md documents `%s`, but this gate could not resolve the Makefile's "+
				"package selector to check it: %v", cmd, err)
			return nil, false
		}
		return []string{pat}, true
	}

	// `go test [flags] [patterns...]` -- keep the arguments that are patterns.
	var patterns []string
	for _, f := range fields[2:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		if strings.HasPrefix(f, "$") || strings.ContainsAny(f, "$()") {
			// A shell substitution (CI's `go test $pkgs` form). Not resolvable
			// here, and not a claim about coverage.
			return nil, false
		}
		patterns = append(patterns, f)
	}
	if len(patterns) == 0 {
		return nil, false
	}
	return patterns, true
}

// makefileAllPkgs reads the selector `make test` actually passes to `go test`,
// rather than restating it. A second copy here would be free to disagree with
// the Makefile, and the disagreement would be invisible.
func makefileAllPkgs() (string, error) {
	b, err := os.ReadFile("Makefile")
	if err != nil {
		return "", err
	}
	vars := map[string]string{}
	assign := regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)\s*:?=\s*(.+?)\s*$`)
	for _, line := range strings.Split(string(b), "\n") {
		if m := assign.FindStringSubmatch(line); m != nil {
			if _, seen := vars[m[1]]; !seen {
				vars[m[1]] = m[2]
			}
		}
	}
	all, ok := vars["ALL_PKGS"]
	if !ok {
		return "", errNoAllPkgs
	}
	// Expand $(VAR) one level, which is all ALL_PKGS needs.
	expanded := regexp.MustCompile(`\$\(([A-Z_][A-Z0-9_]*)\)`).ReplaceAllStringFunc(all, func(ref string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(ref, "$("), ")")
		return vars[name]
	})
	if strings.Contains(expanded, "$") {
		return "", errUnexpandedAllPkgs
	}
	return expanded, nil
}

var (
	errNoAllPkgs         = errAllPkgs("Makefile has no ALL_PKGS assignment")
	errUnexpandedAllPkgs = errAllPkgs("ALL_PKGS still contains an unexpanded variable")
)

type errAllPkgs string

func (e errAllPkgs) Error() string { return string(e) }

// goList resolves patterns to a set of import paths.
//
// A non-zero exit is FATAL rather than tolerated: `go list` prints what it could
// load on stdout while still failing, so a swallowed status leaves a truncated
// list that would read here as "the documented command misses the engine" -- a
// confident, wrong accusation. The same reasoning scripts/ci/db-gated-packages.sh
// records for its own pipeline.
func goList(t *testing.T, patterns []string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, patterns...)...).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("go list %s: %v\n%s", strings.Join(patterns, " "), err, stderr)
	}
	pkgs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs[line] = true
		}
	}
	return pkgs
}
